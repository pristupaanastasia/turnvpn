package turnvpn

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"

	"github.com/pristupaanastasia/turnvpn/internal/vpntun"
)

// The tunnel carries TCP only, so the device's plain UDP DNS queries would be
// dropped. dnsPacketConn bridges a single UDP DNS flow onto DNS-over-TCP
// (RFC 7766: a 2-byte length prefix before each message) dialed through the
// tunnel, so name resolution works without the user enabling Private DNS.
type dnsPacketConn struct {
	dialer *vpntun.Dialer
	dst    string   // DNS server "ip:53" to dial over the tunnel
	server net.Addr // address reported back for each reply

	respCh chan []byte
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	deadline time.Time
}

func newDNSPacketConn(d *vpntun.Dialer, m *M.Metadata) *dnsPacketConn {
	return &dnsPacketConn{
		dialer: d,
		dst:    m.DestinationAddress(),
		server: m.UDPAddr(),
		respCh: make(chan []byte, 8),
		closed: make(chan struct{}),
	}
}

// WriteTo takes one UDP DNS query and resolves it over TCP asynchronously; the
// reply is delivered through ReadFrom. tun2socks may reuse p, so it is copied.
func (c *dnsPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	q := make([]byte, len(p))
	copy(q, p)
	go c.resolve(q)
	return len(p), nil
}

func (c *dnsPacketConn) resolve(q []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.dialer.Dial(ctx, "tcp", c.dst)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	buf := make([]byte, 2+len(q))
	buf[0] = byte(len(q) >> 8)
	buf[1] = byte(len(q))
	copy(buf[2:], q)
	if _, err := conn.Write(buf); err != nil {
		return
	}

	var l [2]byte
	if _, err := io.ReadFull(conn, l[:]); err != nil {
		return
	}
	n := int(l[0])<<8 | int(l[1])
	if n <= 0 || n > 65535 {
		return
	}
	resp := make([]byte, n)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return
	}
	select {
	case c.respCh <- resp:
	case <-c.closed:
	}
}

// ReadFrom returns the next DNS reply, honouring any read deadline tun2socks
// sets to expire the idle UDP session.
func (c *dnsPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.mu.Lock()
	dl := c.deadline
	c.mu.Unlock()

	var timeout <-chan time.Time
	if !dl.IsZero() {
		t := time.NewTimer(time.Until(dl))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case resp := <-c.respCh:
		n := copy(p, resp)
		return n, c.server, nil
	case <-timeout:
		return 0, nil, os.ErrDeadlineExceeded
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *dnsPacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *dnsPacketConn) LocalAddr() net.Addr { return &net.UDPAddr{IP: net.IPv4zero} }

func (c *dnsPacketConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *dnsPacketConn) SetReadDeadline(t time.Time) error  { return c.SetDeadline(t) }
func (c *dnsPacketConn) SetWriteDeadline(t time.Time) error { return nil }
