package main

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/wgclient"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

// TestServeIntegration runs the real serve() over an in-memory carrier with a real
// WireGuard client: it brings a tunnel up (handshake over the carrier) and then
// proves the idle watchdog reclaims it and serve() returns once the client goes
// quiet. The exit/forwarding path itself is covered by the internal/wggw e2e tests.
func TestServeIntegration(t *testing.T) {
	serverPriv, _, _ := wgkey.Generate()
	serverPub, _ := wgkey.Public(serverPriv)
	clientPriv, clientPub, _ := wgkey.Generate()
	const clientIP = "10.6.0.2"

	a, b := dgpipe.Pair()
	done := make(chan struct{})
	go func() {
		// Short idle so the watchdog fires soon after the client goes quiet.
		serve(a, exitCfg{
			mode:   "userspace",
			wgPriv: serverPriv,
			peers:  []wggw.Peer{{PubKey: clientPub, AllowedIP: clientIP + "/32"}},
			mtu:    1420,
			idle:   600 * time.Millisecond,
		})
		close(done)
	}()

	cli, err := wgclient.New(b, wgclient.Config{
		PrivKey: clientPriv, ServerPub: serverPub, Address: clientIP, Keepalive: 1,
	})
	if err != nil {
		t.Fatalf("wgclient: %v", err)
	}
	// Give the handshake time to complete (serve logs "tunnel up").
	time.Sleep(300 * time.Millisecond)

	// Go quiet; the watchdog must tear the tunnel down and serve() must return.
	_ = cli.Close()
	_ = b.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve() did not return after the client went idle")
	}
}

// TestServeKernelExit runs serve() in kernel-exit mode over an in-memory carrier
// against a fake kernel WireGuard socket (a UDP echo). It proves serve relays a
// datagram both ways through the kernel exit and that the idle watchdog reclaims
// the tunnel once the client goes quiet. The real wg0/nft plane is exercised on
// the VPS, not here.
func TestServeKernelExit(t *testing.T) {
	// Fake kernel wg: echo each datagram back with an "R:" prefix.
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("fake kernel wg: %v", err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDP(append([]byte("R:"), buf[:n]...), from)
		}
	}()
	port := pc.LocalAddr().(*net.UDPAddr).Port

	a, b := dgpipe.Pair()
	done := make(chan struct{})
	go func() {
		serve(a, exitCfg{mode: "kernel", idle: 600 * time.Millisecond, wgPort: port})
		close(done)
	}()

	// A client WireGuard datagram must reach the fake kernel and echo back.
	msg := []byte("wg-packet")
	if _, err := b.Write(msg); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 2048)
	n, err := b.Read(got)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if want := append([]byte("R:"), msg...); !bytes.Equal(got[:n], want) {
		t.Fatalf("kernel exit round-trip: got %q want %q", got[:n], want)
	}

	// Go quiet; the watchdog must tear the tunnel down and serve() must return.
	_ = b.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve() did not return after the client went idle (kernel exit)")
	}
}
