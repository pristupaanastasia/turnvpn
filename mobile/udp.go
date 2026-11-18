package turnvpn

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

// udpConn bridges one tun2socks UDP flow onto a tunnel stream: each datagram is
// framed with a uint16 length prefix, and the server relays it to the real UDP
// target. tun2socks drives it as a net.PacketConn.
type udpConn struct {
	stream net.Conn
	server net.Addr
	wmu    sync.Mutex
}

func newUDPConn(stream net.Conn, server net.Addr) *udpConn {
	return &udpConn{stream: stream, server: server}
}

// WriteTo frames p (a single datagram) and sends it down the stream. The addr is
// ignored: the stream is already bound to the flow's destination.
func (c *udpConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	n := len(p)
	if n > 65535 {
		n = 65535
	}
	out := make([]byte, 2+n)
	binary.BigEndian.PutUint16(out[:2], uint16(n))
	copy(out[2:], p[:n])
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.stream.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// ReadFrom returns the next datagram relayed back from the target.
func (c *udpConn) ReadFrom(p []byte) (int, net.Addr, error) {
	var lb [2]byte
	if _, err := io.ReadFull(c.stream, lb[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(lb[:]))
	if n == 0 {
		return 0, c.server, nil
	}
	if len(p) >= n {
		if _, err := io.ReadFull(c.stream, p[:n]); err != nil {
			return 0, nil, err
		}
		return n, c.server, nil
	}
	// Caller buffer too small: drain the datagram, then truncate.
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.stream, buf); err != nil {
		return 0, nil, err
	}
	return copy(p, buf), c.server, nil
}

func (c *udpConn) Close() error                      { return c.stream.Close() }
func (c *udpConn) LocalAddr() net.Addr               { return &net.UDPAddr{IP: net.IPv4zero} }
func (c *udpConn) SetDeadline(t time.Time) error     { return c.stream.SetDeadline(t) }
func (c *udpConn) SetReadDeadline(t time.Time) error { return c.stream.SetReadDeadline(t) }
func (c *udpConn) SetWriteDeadline(t time.Time) error {
	return c.stream.SetWriteDeadline(t)
}
