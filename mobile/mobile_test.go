package turnvpn

import (
	"net"
	"testing"
	"time"
)

func TestHello(t *testing.T) {
	if Hello() == "" {
		t.Error("Hello returned empty")
	}
}

func TestUDPConnMeta(t *testing.T) {
	c := newUDPConn(nil, &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5})
	if err := c.SetDeadline(time.Now()); err != nil {
		t.Error(err)
	}
	if err := c.SetReadDeadline(time.Now()); err != nil {
		t.Error(err)
	}
	if err := c.SetWriteDeadline(time.Now()); err != nil {
		t.Error(err)
	}
	if c.LocalAddr() == nil {
		t.Error("LocalAddr nil")
	}
}

func TestDNSConnMeta(t *testing.T) {
	c := &dnsPacketConn{closed: make(chan struct{})}
	_ = c.SetDeadline(time.Now())
	_ = c.SetReadDeadline(time.Now())
	_ = c.SetWriteDeadline(time.Now())
	if c.LocalAddr() == nil {
		t.Error("LocalAddr nil")
	}
	if err := c.Close(); err != nil {
		t.Error(err)
	}
	// Close is idempotent (second call must not panic on the closed channel).
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}
