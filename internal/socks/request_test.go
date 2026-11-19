package socks

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// requestWith runs Request on one end of a loopback TCP pair while the other end
// sends a full SOCKS5 greeting followed by a request.
func requestWith(t *testing.T, greetingAndRequest []byte) (byte, string, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	type res struct {
		cmd    byte
		target string
		err    error
	}
	ch := make(chan res, 1)
	go func() {
		srv, err := ln.Accept()
		if err != nil {
			ch <- res{0, "", err}
			return
		}
		defer func() { _ = srv.Close() }()
		cmd, target, err := Request(srv)
		ch <- res{cmd, target, err}
	}()
	cl, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cl.Close() }()
	if _, err := cl.Write(greetingAndRequest); err != nil {
		t.Fatal(err)
	}
	// Half-close so a truncated request reaches the server as EOF rather than
	// blocking Request's ReadFull forever.
	if tcp, ok := cl.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	select {
	case r := <-ch:
		return r.cmd, r.target, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Request did not return")
		return 0, "", nil
	}
}

// req builds greeting(no-auth) + request header(ver,cmd,rsv) + the given tail.
func req(cmd byte, tail ...byte) []byte {
	return append([]byte{0x05, 0x01, 0x00, version, cmd, 0x00}, tail...)
}

func TestRequestUDPAssociateIPv4(t *testing.T) {
	cmd, target, err := requestWith(t, req(CmdUDPAssociate, atypIPv4, 10, 0, 0, 1, 0x1f, 0x90))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if cmd != CmdUDPAssociate || target != "10.0.0.1:8080" {
		t.Fatalf("cmd=%d target=%q", cmd, target)
	}
}

func TestRequestConnectIPv6(t *testing.T) {
	ip6 := net.ParseIP("::1").To16()
	tail := append([]byte{atypIPv6}, ip6...)
	tail = append(tail, 0x00, 0x50) // port 80
	cmd, target, err := requestWith(t, req(CmdConnect, tail...))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if cmd != CmdConnect || target != "[::1]:80" {
		t.Fatalf("cmd=%d target=%q", cmd, target)
	}
}

func TestRequestConnectDomain(t *testing.T) {
	host := "example.com"
	tail := append([]byte{atypHost, byte(len(host))}, host...)
	tail = append(tail, 0x01, 0xbb) // port 443
	_, target, err := requestWith(t, req(CmdConnect, tail...))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if target != "example.com:443" {
		t.Fatalf("target=%q", target)
	}
}

func TestRequestErrors(t *testing.T) {
	// Unknown ATYP.
	if _, _, err := requestWith(t, req(CmdConnect, 0x09, 1, 2, 3, 4)); err == nil {
		t.Error("bad atyp: expected error")
	}
	// Truncated IPv4 address.
	if _, _, err := requestWith(t, req(CmdConnect, atypIPv4, 10, 0)); err == nil {
		t.Error("short addr: expected error")
	}
	// Not SOCKS5 greeting.
	if _, _, err := requestWith(t, []byte{0x04, 0x01, 0x00}); err == nil {
		t.Error("bad version: expected error")
	}
}

func tcpPair(t *testing.T) (srv, cl net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		done <- c
	}()
	cl, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv = <-done
	t.Cleanup(func() { _ = srv.Close(); _ = cl.Close() })
	return srv, cl
}

func TestReplyConnect(t *testing.T) {
	srv, cl := tcpPair(t)
	if err := ReplyConnect(srv); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(cl, buf); err != nil {
		t.Fatal(err)
	}
	want := []byte{version, 0x00, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(buf, want) {
		t.Fatalf("reply = %v, want %v", buf, want)
	}
}

func TestReplyUDPAssociate(t *testing.T) {
	srv, cl := tcpPair(t)
	relay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}
	if err := ReplyUDPAssociate(srv, relay); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10) // ver,rsv,rsv,atyp + 4 (ipv4) + 2 (port)
	if _, err := io.ReadFull(cl, buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != version || buf[3] != atypIPv4 {
		t.Fatalf("header = %v", buf[:4])
	}
	port := int(buf[8])<<8 | int(buf[9])
	if port != 9999 {
		t.Fatalf("port = %d, want 9999", port)
	}

	// IPv6 relay path.
	srv6, cl6 := tcpPair(t)
	relay6 := &net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 1}
	if err := ReplyUDPAssociate(srv6, relay6); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(cl6, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[3] != atypIPv6 {
		t.Fatalf("expected IPv6 atyp, got %d", hdr[3])
	}
}

func TestEncodeUDPBadSource(t *testing.T) {
	if _, err := EncodeUDP("no-port-here", []byte("x")); err == nil {
		t.Error("EncodeUDP with a bad source should error")
	}
}

func TestWriteReadTargetLongHost(t *testing.T) {
	target := strings.Repeat("a", 300) + ":80"
	var b bytes.Buffer
	if err := WriteTarget(&b, target); err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	got, err := ReadTarget(&b)
	if err != nil {
		t.Fatalf("ReadTarget: %v", err)
	}
	if got != target {
		t.Fatalf("round trip = %q, want %q", got, target)
	}
}

func TestRequestBadRequestVersion(t *testing.T) {
	// Valid greeting, then a request whose version byte is not 5.
	if _, _, err := requestWith(t, []byte{0x05, 0x01, 0x00, 0x04, 0x01, 0x00, 0x01, 1, 2, 3, 4, 0, 80}); err == nil {
		t.Error("bad request version should error")
	}
}
