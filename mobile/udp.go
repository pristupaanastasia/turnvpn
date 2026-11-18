package turnvpn

import (
	"net"
	"os"
	"sync"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/vpntun"
)

// udpConn presents one vpntun UDP flow (datagrams over the tunnel's unreliable Lane)
// as a net.PacketConn for tun2socks. Unlike the old stream-based path, datagrams are
// not forced through a reliable ordered stream, so a lost packet does not head-of-line
// block the flow (QUIC keeps its semantics).
type udpConn struct {
	flow   *vpntun.UDPFlow
	server net.Addr

	mu     sync.Mutex
	rdl    time.Time
	once   sync.Once
	closed chan struct{}
}

func newUDPConn(flow *vpntun.UDPFlow, server net.Addr) *udpConn {
	return &udpConn{flow: flow, server: server, closed: make(chan struct{})}
}

func (c *udpConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.flow.Send(p)
}

func (c *udpConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.mu.Lock()
	dl := c.rdl
	c.mu.Unlock()

	var timeout <-chan time.Time
	if !dl.IsZero() {
		t := time.NewTimer(time.Until(dl))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case b := <-c.flow.Inbound():
		return copy(p, b), c.server, nil
	case <-timeout:
		return 0, nil, os.ErrDeadlineExceeded
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *udpConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.flow.Close()
}

func (c *udpConn) LocalAddr() net.Addr { return &net.UDPAddr{IP: net.IPv4zero} }

func (c *udpConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

func (c *udpConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rdl = t
	c.mu.Unlock()
	return nil
}

func (c *udpConn) SetWriteDeadline(_ time.Time) error { return nil }
