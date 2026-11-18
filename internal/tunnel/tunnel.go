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

	"github.com/pristupaanastasia/turnvpn/internal/mux"
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

// Serve accepts streams on sess until it closes. A TCP stream is piped to a dialed
// peer; a "udp/" control stream registers a flow whose datagrams ride lane (the
// unreliable channel) instead of the ordered stream — so UDP/QUIC is not head-of-line
// blocked by the reliable path. lane may be nil (UDP then refused). logf may be nil.
func Serve(sess *smux.Session, lane *mux.Lane, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	reg := newUDPRegistry()
	defer reg.closeAll()
	if lane != nil {
		lane.SetDatagramHandler(func(flow uint32, payload []byte) { reg.send(flow, payload) })
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
			handle(s, lane, reg, logf)
		}(stream)
	}
	wg.Wait()
}

// handle reads one stream's target header and either pipes it to a dialed TCP peer
// or (for a "udp/" stream) sets up a UDP flow.
func handle(s *smux.Stream, lane *mux.Lane, reg *udpRegistry, logf func(string, ...any)) {
	defer func() { _ = s.Close() }()

	target, err := socks.ReadTarget(s)
	if err != nil {
		logf("tunnel: stream %d: read target: %v", s.ID(), err)
		return
	}
	if strings.HasPrefix(target, udpScheme) {
		handleUDP(s, strings.TrimPrefix(target, udpScheme), lane, reg, logf)
		return
	}
	addr, err := resolveSafe(target)
	if err != nil {
		logf("tunnel: %v", err)
		return
	}
	remote, err := net.DialTimeout("tcp", addr, DialTimeout)
	if err != nil {
		logf("tunnel: dial %s: %v", addr, err)
		return
	}
	defer func() { _ = remote.Close() }()
	Pipe(s, remote)
}

// handleUDP sets up a UDP flow: it reads the client's 4-byte flow id from the
// control stream, dials dst over UDP, and relays datagrams over lane (socket -> lane
// here; lane -> socket via the registry handler). The control stream stays open for
// the flow's lifetime; its EOF — or dst going idle — tears the flow down.
func handleUDP(s *smux.Stream, dst string, lane *mux.Lane, reg *udpRegistry, logf func(string, ...any)) {
	if lane == nil {
		logf("tunnel: udp flow but no datagram lane")
		return
	}
	var fb [4]byte
	if _, err := io.ReadFull(s, fb[:]); err != nil {
		logf("tunnel: udp flow id: %v", err)
		return
	}
	flow := binary.BigEndian.Uint32(fb[:])

	addr, err := resolveSafe(dst)
	if err != nil {
		logf("tunnel: udp %v", err)
		return
	}
	uc, err := net.DialTimeout("udp", addr, DialTimeout)
	if err != nil {
		logf("tunnel: udp dial %s: %v", addr, err)
		return
	}
	reg.add(flow, uc)
	defer reg.remove(flow) // closes uc

	// socket -> lane: tag each inbound datagram with its flow id.
	go func() {
		buf := make([]byte, maxDatagram)
		for {
			_ = uc.SetReadDeadline(time.Now().Add(udpIdle))
			n, err := uc.Read(buf)
			if err != nil {
				_ = s.Close() // wake the control-stream reader -> teardown
				return
			}
			if _, err := lane.SendDatagram(flow, buf[:n]); err != nil {
				return
			}
		}
	}()

	// Block until the client closes the control stream (flow teardown).
	_, _ = io.Copy(io.Discard, s)
}

// udpRegistry maps a flow id to its UDP socket for one served session.
type udpRegistry struct {
	mu sync.Mutex
	m  map[uint32]net.Conn
}

func newUDPRegistry() *udpRegistry { return &udpRegistry{m: make(map[uint32]net.Conn)} }

func (r *udpRegistry) add(flow uint32, c net.Conn) {
	r.mu.Lock()
	if old := r.m[flow]; old != nil {
		_ = old.Close()
	}
	r.m[flow] = c
	r.mu.Unlock()
}

func (r *udpRegistry) remove(flow uint32) {
	r.mu.Lock()
	c := r.m[flow]
	delete(r.m, flow)
	r.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (r *udpRegistry) send(flow uint32, b []byte) {
	r.mu.Lock()
	c := r.m[flow]
	r.mu.Unlock()
	if c != nil {
		_, _ = c.Write(b)
	}
}

func (r *udpRegistry) closeAll() {
	r.mu.Lock()
	for _, c := range r.m {
		_ = c.Close()
	}
	r.m = make(map[uint32]net.Conn)
	r.mu.Unlock()
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

// OpenUDP opens a UDP control stream for a flow: it writes the "udp/"+target header
// and the 4-byte flow id. Datagrams themselves ride the Lane (keyed by flow id), not
// this stream; the stream stays open as the flow's lifetime (closing it tears the
// flow down server-side).
func OpenUDP(sess *smux.Session, target string, flow uint32) (*smux.Stream, error) {
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := socks.WriteTarget(stream, udpScheme+target); err != nil {
		_ = stream.Close()
		return nil, err
	}
	var fb [4]byte
	binary.BigEndian.PutUint32(fb[:], flow)
	if _, err := stream.Write(fb[:]); err != nil {
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
