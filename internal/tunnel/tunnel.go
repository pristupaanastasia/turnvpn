// Package tunnel carries SOCKS-style streams over a smux session: the server side
// accepts streams, reads each stream's "host:port" target and pipes it to a freshly
// dialed TCP connection; the client side opens a stream and writes the target. It is
// the shared data path of cmd/server and cmd/client, and is exercised end-to-end
// (DTLS+KCP+smux+dial+pipe) in the package's integration test.
package tunnel

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/xtaci/smux"

	"github.com/pristupaanastasia/turnvpn/internal/socks"
)

// DialTimeout bounds how long the server waits to reach a stream's target.
var DialTimeout = 10 * time.Second

// udpIdle bounds how long a UDP association waits with no inbound datagram before
// it is torn down (so forgotten flows don't leak sockets).
var udpIdle = 60 * time.Second

// udpScheme marks a stream as a UDP association (vs the default TCP connect). The
// client prefixes the target with it; the server strips it and dials UDP. TCP
// streams are unprefixed, so an older TCP-only server stays wire-compatible.
const udpScheme = "udp/"

// maxDatagram bounds one UDP payload (the uint16 length prefix framing datagrams).
const maxDatagram = 65535

// Serve accepts streams on sess until it closes; each stream's target is read and
// piped to a freshly dialed TCP connection. logf may be nil.
func Serve(sess *smux.Session, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var wg sync.WaitGroup
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			break // session closed
		}
		wg.Add(1)
		go func(s *smux.Stream) {
			defer wg.Done()
			handle(s, logf)
		}(stream)
	}
	wg.Wait()
}

// handle reads one stream's target header and pipes it to a dialed TCP peer.
func handle(s *smux.Stream, logf func(string, ...any)) {
	defer func() { _ = s.Close() }()

	target, err := socks.ReadTarget(s)
	if err != nil {
		logf("tunnel: stream %d: read target: %v", s.ID(), err)
		return
	}
	if strings.HasPrefix(target, udpScheme) {
		handleUDP(s, strings.TrimPrefix(target, udpScheme), logf)
		return
	}
	remote, err := net.DialTimeout("tcp", target, DialTimeout)
	if err != nil {
		logf("tunnel: dial %s: %v", target, err)
		return
	}
	defer func() { _ = remote.Close() }()
	Pipe(s, remote)
}

// handleUDP relays a UDP association: it dials dst over UDP and pipes length-prefixed
// datagrams between the stream and the socket until either side ends or dst goes idle.
func handleUDP(s *smux.Stream, dst string, logf func(string, ...any)) {
	uc, err := net.DialTimeout("udp", dst, DialTimeout)
	if err != nil {
		logf("tunnel: udp dial %s: %v", dst, err)
		return
	}
	defer func() { _ = uc.Close() }()

	// stream -> socket: each datagram is a uint16 length then that many bytes.
	go func() {
		defer func() { _ = uc.Close(); _ = s.Close() }()
		var lb [2]byte
		for {
			if _, err := io.ReadFull(s, lb[:]); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint16(lb[:]))
			if n == 0 {
				continue
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(s, buf); err != nil {
				return
			}
			if _, err := uc.Write(buf); err != nil {
				return
			}
		}
	}()

	// socket -> stream: frame each datagram with a uint16 length; idle tears it down.
	buf := make([]byte, maxDatagram)
	for {
		_ = uc.SetReadDeadline(time.Now().Add(udpIdle))
		n, err := uc.Read(buf)
		if err != nil {
			return
		}
		out := make([]byte, 2+n)
		binary.BigEndian.PutUint16(out[:2], uint16(n))
		copy(out[2:], buf[:n])
		if _, err := s.Write(out); err != nil {
			return
		}
	}
}

// Open opens a new stream on sess for target and writes the target header; the
// returned stream is ready to be piped to the local connection.
func Open(sess *smux.Session, target string) (*smux.Stream, error) {
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := socks.WriteTarget(stream, target); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

// OpenUDP opens a stream that relays UDP datagrams to target: the server dials UDP
// and pipes datagrams both ways. The caller frames each datagram with a uint16
// length prefix (see the mobile udpConn). TCP streams stay unprefixed.
func OpenUDP(sess *smux.Session, target string) (*smux.Stream, error) {
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := socks.WriteTarget(stream, udpScheme+target); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

// Pipe copies bytes both ways between a and b until either side ends, then closes
// both so the other direction unblocks.
func Pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
