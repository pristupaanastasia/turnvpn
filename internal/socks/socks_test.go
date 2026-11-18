package socks

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// handshakeWith runs Handshake on one end of a loopback TCP pair while the other end
// sends greetingAndRequest (a full SOCKS5 no-auth greeting followed by a request).
// TCP (OS-buffered) is used instead of net.Pipe so the small reply writes from
// Handshake never block, keeping the test free of synchronous deadlocks.
func handshakeWith(t *testing.T, greetingAndRequest []byte) (string, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	type result struct {
		target string
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		srv, err := ln.Accept()
		if err != nil {
			ch <- result{"", err}
			return
		}
		defer func() { _ = srv.Close() }()
		target, err := Handshake(srv)
		ch <- result{target, err}
	}()

	cl, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cl.Close() }()
	if _, err := cl.Write(greetingAndRequest); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-ch:
		return r.target, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Handshake did not return")
		return "", nil
	}
}

// greeting is the SOCKS5 no-auth client greeting (VER, NMETHODS=1, METHOD=0).
var greeting = []byte{0x05, 0x01, 0x00}

func TestHandshakeDomain(t *testing.T) {
	host := "example.com"
	req := append([]byte{0x05, 0x01, 0x00, atypHost, byte(len(host))}, host...)
	req = append(req, 0x01, 0xBB) // port 443
	got, err := handshakeWith(t, append(append([]byte{}, greeting...), req...))
	if err != nil {
		t.Fatal(err)
	}
	if got != "example.com:443" {
		t.Fatalf("target = %q, want example.com:443", got)
	}
}

func TestHandshakeIPv4(t *testing.T) {
	req := []byte{0x05, 0x01, 0x00, atypIPv4, 10, 0, 0, 7, 0x1F, 0x90} // 10.0.0.7:8080
	got, err := handshakeWith(t, append(append([]byte{}, greeting...), req...))
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.0.0.7:8080" {
		t.Fatalf("target = %q, want 10.0.0.7:8080", got)
	}
}

func TestHandshakeRejectsNonConnect(t *testing.T) {
	req := []byte{0x05, 0x02, 0x00, atypIPv4, 1, 2, 3, 4, 0, 80} // CMD=2 (BIND)
	if _, err := handshakeWith(t, append(append([]byte{}, greeting...), req...)); err == nil {
		t.Fatal("expected error for non-CONNECT command")
	}
}

func TestHandshakeRejectsBadVersion(t *testing.T) {
	if _, err := handshakeWith(t, []byte{0x04, 0x01, 0x00}); err == nil { // SOCKS4
		t.Fatal("expected error for non-SOCKS5 greeting")
	}
}

func TestTargetHeaderRoundTrip(t *testing.T) {
	for _, target := range []string{"example.com:443", "10.0.0.7:8080", "[::1]:53"} {
		var buf bytes.Buffer
		if err := WriteTarget(&buf, target); err != nil {
			t.Fatalf("WriteTarget(%q): %v", target, err)
		}
		got, err := ReadTarget(&buf)
		if err != nil {
			t.Fatalf("ReadTarget(%q): %v", target, err)
		}
		if got != target {
			t.Errorf("round-trip = %q, want %q", got, target)
		}
	}
}

func TestReadTargetRejectsGarbageLength(t *testing.T) {
	bad := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F} // ~4 GiB uvarint
	if _, err := ReadTarget(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error for oversized target length")
	}
}
