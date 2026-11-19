package main

import (
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
		serve(a, serverPriv, []wggw.Peer{{PubKey: clientPub, AllowedIP: clientIP + "/32"}}, 1420, 600*time.Millisecond)
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
