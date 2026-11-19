package wgclient_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/wgclient"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

// TestClientDialThroughGateway wires the real client package (wgclient) to the real
// server gateway (wggw) over an in-memory carrier and drives an HTTP request through
// wgclient.DialContext — the same call the SOCKS front-end makes. It proves the
// client's UAPI config and dial path, end to end to a real host server.
func TestClientDialThroughGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "exit-ok-client")
	}))
	defer srv.Close()
	realTarget := srv.Listener.Addr().String()
	const routedTarget = "192.0.2.30:80"

	serverPriv, serverPub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	clientPriv, clientPub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	const clientIP = "10.6.0.2"

	cliCarrier, srvCarrier := dgpipe.Pair()

	gw, err := wggw.New(srvCarrier, wggw.Config{
		ServerPrivKey: serverPriv,
		Peers:         []wggw.Peer{{PubKey: clientPub, AllowedIP: clientIP + "/32"}},
		MTU:           1420,
		Logf:          t.Logf,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != routedTarget {
				return nil, fmt.Errorf("unexpected dial target %q", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", realTarget)
		},
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	defer gw.Close()
	defer srvCarrier.Close()

	cli, err := wgclient.New(cliCarrier, wgclient.Config{
		PrivKey:   clientPriv,
		ServerPub: serverPub,
		Address:   clientIP,
		MTU:       1420,
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatalf("wgclient: %v", err)
	}
	defer cli.Close()
	defer cliCarrier.Close()

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DialContext: cli.DialContext},
	}
	resp, err := client.Get("http://" + routedTarget + "/")
	if err != nil {
		t.Fatalf("GET through wgclient: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != "exit-ok-client" {
		t.Fatalf("body = %q, want %q", got, "exit-ok-client")
	}
}
