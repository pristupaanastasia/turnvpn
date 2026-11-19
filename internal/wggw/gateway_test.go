package wggw_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
)

func wgKeypair(t *testing.T) (priv, pub string) {
	t.Helper()
	var sk [32]byte
	if _, err := io.ReadFull(rand.Reader, sk[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sk[0] &= 248
	sk[31] &= 127
	sk[31] |= 64
	pk, err := curve25519.X25519(sk[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	return hex.EncodeToString(sk[:]), hex.EncodeToString(pk)
}

// TestGatewayExit drives the full server-side exit path locally, no VPS and no
// root: a wireguard-go client on a netstack TUN sends over an in-memory carrier
// (the DTLS-over-TURN stand-in) to a wggw.Gateway, which forwards the flow out to
// a real HTTP server on the host. So the path is HTTP → WG encrypt → carrier → WG
// decrypt → gVisor forwarder → host net.Dial → HTTP server, exactly the shape the
// VPS will run.
func TestGatewayExit(t *testing.T) {
	// A real host-side HTTP server; the gateway dials it like any internet target.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "exit-ok")
	}))
	defer srv.Close()
	realTarget := srv.Listener.Addr().String() // 127.0.0.1:PORT (host loopback)

	// The client routes to a non-loopback documentation address (TEST-NET-1): a
	// gVisor gateway drops martian/loopback destinations, and real internet targets
	// are never loopback. The gateway's Dial maps that target to the real host
	// server, standing in for "dial whatever the client asked for".
	const routedTarget = "192.0.2.10:80"

	serverPriv, serverPub := wgKeypair(t)
	clientPriv, clientPub := wgKeypair(t)
	const clientIP = "10.6.0.2"

	cliCarrier, srvCarrier := dgpipe.Pair()

	// --- server: WireGuard exit gateway over srvCarrier ---
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

	// --- client: wireguard-go + netstack TUN over cliCarrier ---
	cliTun, cliNet, err := netstack.CreateNetTUN(
		[]netip.Addr{netip.MustParseAddr(clientIP)}, nil, 1420)
	if err != nil {
		t.Fatalf("client tun: %v", err)
	}
	cliDev := device.NewDevice(cliTun, wgbind.New(cliCarrier), device.NewLogger(device.LogLevelError, "wg-client "))
	cliCfg := fmt.Sprintf(""+
		"private_key=%s\n"+
		"listen_port=0\n"+
		"replace_peers=true\n"+
		"public_key=%s\n"+
		"endpoint=127.0.0.1:1\n"+
		"allowed_ip=0.0.0.0/0\n"+
		"persistent_keepalive_interval=1\n",
		clientPriv, serverPub)
	if err := cliDev.IpcSet(cliCfg); err != nil {
		t.Fatalf("client ipc: %v", err)
	}
	if err := cliDev.Up(); err != nil {
		t.Fatalf("client up: %v", err)
	}
	defer cliDev.Close()
	defer cliCarrier.Close()

	// Dial the host HTTP server *through the tunnel*: the client's netstack routes
	// the target IP via 0.0.0.0/0 into WireGuard, and the gateway dials it out.
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return cliNet.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort(routedTarget))
			},
		},
	}

	resp, err := client.Get("http://" + routedTarget + "/")
	if err != nil {
		t.Fatalf("GET through wireguard gateway: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != "exit-ok" {
		t.Fatalf("body = %q, want %q", got, "exit-ok")
	}
}
