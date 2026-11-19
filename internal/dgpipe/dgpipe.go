// Package dgpipe is an in-memory, datagram-preserving net.Conn pair for tests.
//
// Unlike net.Pipe (a byte stream), each Write is delivered to the peer as exactly
// one Read, so it stands in for a message-preserving transport such as the
// DTLS-over-TURN carrier. It honours read deadlines — including waking a read
// already in flight — because wgbind.Bind interrupts its receive via
// SetReadDeadline, exactly as a real pion/dtls conn does.
package dgpipe

import (
	"net"
	"os"
	"sync"
	"time"
)

type conn struct {
	rx     <-chan []byte
	tx     chan<- []byte
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	deadline time.Time
	dlChange chan struct{} // closed whenever the read deadline changes
}

// Pair returns two connected endpoints. A Write on one is one Read on the other.
func Pair() (net.Conn, net.Conn) {
	c1 := make(chan []byte, 256)
	c2 := make(chan []byte, 256)
	a := &conn{rx: c1, tx: c2, closed: make(chan struct{}), dlChange: make(chan struct{})}
	b := &conn{rx: c2, tx: c1, closed: make(chan struct{}), dlChange: make(chan struct{})}
	return a, b
}

func (m *conn) Read(p []byte) (int, error) {
	m.mu.Lock()
	dl := m.deadline
	change := m.dlChange
	m.mu.Unlock()

	var timer <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}

	select {
	case b := <-m.rx:
		return copy(p, b), nil
	case <-m.closed:
		return 0, net.ErrClosed
	case <-timer:
		return 0, os.ErrDeadlineExceeded
	case <-change:
		return m.Read(p) // deadline moved while blocked — re-evaluate
	}
}

func (m *conn) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case m.tx <- b:
		return len(p), nil
	case <-m.closed:
		return 0, net.ErrClosed
	}
}

func (m *conn) Close() error         { m.once.Do(func() { close(m.closed) }); return nil }
func (m *conn) LocalAddr() net.Addr  { return addr{} }
func (m *conn) RemoteAddr() net.Addr { return addr{} }

func (m *conn) SetReadDeadline(t time.Time) error {
	m.mu.Lock()
	m.deadline = t
	close(m.dlChange) // wake any in-flight Read so it picks up the new deadline
	m.dlChange = make(chan struct{})
	m.mu.Unlock()
	return nil
}
func (m *conn) SetDeadline(t time.Time) error    { return m.SetReadDeadline(t) }
func (m *conn) SetWriteDeadline(time.Time) error { return nil }

type addr struct{}

func (addr) Network() string { return "dgpipe" }
func (addr) String() string  { return "dgpipe" }
