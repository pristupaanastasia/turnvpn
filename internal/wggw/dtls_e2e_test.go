package wggw_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

func dtlsConfig() *dtls.Config {
	return &dtls.Config{
		PSK:                  authdtls.PSK("test-secret-entropy-xyz"),
		PSKIdentityHint:      authdtls.IdentityHint(),
		CipherSuites:         []dtls.CipherSuiteID{authdtls.Cipher},
		ExtendedMasterSecret: dtls.RequireExtendedMasterSecret,
	}
}

// TestGatewayOverRealDTLS runs the full WireGuard exit over a *real* pion/dtls
// carrier (PSK mutual auth, over loopback UDP — the same carrier shape as
// DTLS-over-TURN, minus the relay). It exercises what dgpipe cannot: real DTLS
// record framing/sizes and SetReadDeadline on a real pion/dtls conn (which
// wgbind.Bind depends on). Path: HTTP → WG → DTLS → UDP → DTLS → WG → gateway →
// host dial → HTTP.
func TestGatewayOverRealDTLS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "exit-ok-dtls")
	}))
	defer srv.Close()
	realTarget := srv.Listener.Addr().String()
	const routedTarget = "192.0.2.20:80"

	serverPriv, serverPub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	clientPriv, clientPub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	const clientIP = "10.6.0.2"

	// --- DTLS server (carrier listener) on loopback UDP ---
	ln, err := dtls.Listen("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, dtlsConfig())
	if err != nil {
		t.Fatalf("dtls listen: %v", err)
	}
	defer ln.Close()
	serverAddr := ln.Addr().(*net.UDPAddr)

	gwCh := make(chan *wggw.Gateway, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		gw, err := wggw.New(conn, wggw.Config{
			ServerPrivKey: serverPriv,
			Peers:         []wggw.Peer{{PubKey: clientPub, AllowedIP: clientIP + "/32"}},
			MTU:           1280,
			Logf:          t.Logf,
			Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr != routedTarget {
					return nil, fmt.Errorf("unexpected dial target %q", addr)
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp", realTarget)
			},
		})
		if err != nil {
			errCh <- err
			return
		}
		gwCh <- gw
	}()

	// --- DTLS client (carrier) ---
	carrier, err := dtls.Dial("udp", serverAddr, dtlsConfig())
	if err != nil {
		t.Fatalf("dtls dial: %v", err)
	}
	defer carrier.Close()

	// --- WireGuard client over the DTLS carrier, on a netstack TUN ---
	cliTun, cliNet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(clientIP)}, nil, 1280)
	if err != nil {
		t.Fatalf("client tun: %v", err)
	}
	cliDev := device.NewDevice(cliTun, wgbind.New(carrier), device.NewLogger(device.LogLevelError, "wg-client "))
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

	// Ensure the gateway came up (or surface its error) before dialing.
	select {
	case gw := <-gwCh:
		defer gw.Close()
	case err := <-errCh:
		t.Fatalf("gateway: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not start")
	}

	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return cliNet.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort(routedTarget))
			},
		},
	}
	resp, err := client.Get("http://" + routedTarget + "/")
	if err != nil {
		t.Fatalf("GET over wireguard+dtls: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != "exit-ok-dtls" {
		t.Fatalf("body = %q, want %q", got, "exit-ok-dtls")
	}
}
