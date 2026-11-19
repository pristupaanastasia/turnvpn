package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/socks"
	"github.com/pristupaanastasia/turnvpn/internal/wgclient"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

func tcpPair(t *testing.T) (srv, cl net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); done <- c }()
	cl, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv = <-done
	t.Cleanup(func() { _ = srv.Close(); _ = cl.Close() })
	return srv, cl
}

const (
	routedTCP = "192.0.2.50:80"
	routedUDP = "192.0.2.51:9"
)

// buildTunnel wires a real wgclient to a wggw gateway over an in-memory carrier.
// The gateway's Dial maps the routed test targets to the given real addresses
// (dialing the routed 192.0.2.x addresses directly would be unreachable; a
// loopback address is dropped as martian by the gateway's netstack).
func buildTunnel(t *testing.T, tcpTarget, udpTarget string) *tunnel {
	t.Helper()
	serverPriv, _, _ := wgkey.Generate()
	serverPub, _ := wgkey.Public(serverPriv)
	clientPriv, clientPub, _ := wgkey.Generate()
	const clientIP = "10.6.0.2"

	cliC, srvC := dgpipe.Pair()
	gw, err := wggw.New(srvC, wggw.Config{
		ServerPrivKey: serverPriv,
		Peers:         []wggw.Peer{{PubKey: clientPub, AllowedIP: clientIP + "/32"}},
		MTU:           1420,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			switch {
			case network == "tcp" && addr == routedTCP && tcpTarget != "":
				return (&net.Dialer{}).DialContext(ctx, "tcp", tcpTarget)
			case network == "udp" && addr == routedUDP && udpTarget != "":
				return (&net.Dialer{}).DialContext(ctx, "udp", udpTarget)
			}
			return nil, fmt.Errorf("unexpected target %s %q", network, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cli, err := wgclient.New(cliC, wgclient.Config{PrivKey: clientPriv, ServerPub: serverPub, Address: clientIP, Keepalive: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close(); gw.Close(); _ = cliC.Close(); _ = srvC.Close() })

	tn := &tunnel{}
	tn.setCur(&clientRef{client: cli})
	return tn
}

func TestServeConnConnect(t *testing.T) {
	// A tiny origin server reachable through the tunnel.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1024)
				_, _ = c.Read(buf) // read the request line; don't wait for EOF
				_, _ = io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Length: 6\r\n\r\nserved")
				_ = c.Close()
			}()
		}
	}()

	tn := buildTunnel(t, ln.Addr().String(), "")

	browser, server := tcpPair(t)
	go serveConn(tn, server)

	// SOCKS5 greeting + CONNECT 192.0.2.50:80.
	connect := []byte{0x05, 0x01, 0x00, 0x05, 0x01, 0x00, 0x01, 192, 0, 2, 50, 0, 80}
	if _, err := browser.Write(connect); err != nil {
		t.Fatal(err)
	}
	_ = browser.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(browser, make([]byte, 2)); err != nil { // method selection
		t.Fatalf("method selection: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(browser, reply); err != nil {
		t.Fatalf("socks reply: %v", err)
	}
	if reply[1] != 0x00 {
		t.Fatalf("socks reply code = %d", reply[1])
	}
	if _, err := browser.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(browser)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(body), "served") {
		t.Fatalf("response did not contain the origin body: %q", body)
	}
}

func TestServeConnUDPAssociate(t *testing.T) {
	tn := buildTunnel(t, "", "")
	browser, server := tcpPair(t)
	go serveConn(tn, server)

	// SOCKS5 greeting + UDP ASSOCIATE (addr/port 0).
	req := []byte{0x05, 0x01, 0x00, 0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if _, err := browser.Write(req); err != nil {
		t.Fatal(err)
	}
	_ = browser.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(browser, make([]byte, 2)); err != nil { // method selection
		t.Fatalf("method selection: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(browser, reply); err != nil {
		t.Fatalf("udp associate reply: %v", err)
	}
	if reply[1] != 0x00 {
		t.Fatalf("reply code = %d", reply[1])
	}
	// Closing the control connection tears the association down (serveUDPAssociate returns).
	_ = browser.Close()
	time.Sleep(50 * time.Millisecond)
}

func TestServeConnUDPDatagram(t *testing.T) {
	// UDP echo origin reached through the tunnel.
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()

	tn := buildTunnel(t, "", echo.LocalAddr().String())
	browser, server := tcpPair(t)
	go serveConn(tn, server)

	// UDP ASSOCIATE; read the relay address from the reply.
	req := []byte{0x05, 0x01, 0x00, 0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if _, err := browser.Write(req); err != nil {
		t.Fatal(err)
	}
	_ = browser.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(browser, make([]byte, 2)); err != nil { // method selection
		t.Fatalf("method selection: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(browser, reply); err != nil {
		t.Fatalf("associate reply: %v", err)
	}
	relay := &net.UDPAddr{IP: net.IP(reply[4:8]), Port: int(reply[8])<<8 | int(reply[9])}

	// Send a SOCKS-encapsulated datagram to the relay and expect the echo back.
	uc, err := net.DialUDP("udp", nil, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	packet, err := socks.EncodeUDP(routedUDP, []byte("ping"))
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 2048)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := uc.Write(packet); err != nil {
			t.Fatal(err)
		}
		_ = uc.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		n, err := uc.Read(buf)
		if err != nil {
			continue // resend on timeout (first datagram can race socket bind)
		}
		_, payload, derr := socks.DecodeUDP(buf[:n])
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		if string(payload) != "ping" {
			t.Fatalf("echo = %q", payload)
		}
		return
	}
	t.Fatal("no UDP echo through the relay")
}
