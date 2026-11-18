// Package tunnel carries SOCKS-style streams over a smux session: the server side
// accepts streams, reads each stream's "host:port" target and pipes it to a freshly
// dialed TCP connection; the client side opens a stream and writes the target. It is
// the shared data path of cmd/server and cmd/client, and is exercised end-to-end
// (DTLS+KCP+smux+dial+pipe) in the package's integration test.
package tunnel

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/xtaci/smux"

	"github.com/pristupaanastasia/turnvpn/internal/socks"
)

// DialTimeout bounds how long the server waits to reach a stream's target.
var DialTimeout = 10 * time.Second

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
	remote, err := net.DialTimeout("tcp", target, DialTimeout)
	if err != nil {
		logf("tunnel: dial %s: %v", target, err)
		return
	}
	defer func() { _ = remote.Close() }()
	Pipe(s, remote)
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
