package wgbind_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
)

// wgKeypair returns a clamped X25519 private key and its public key, hex-encoded
// as wireguard-go's UAPI expects.
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

// TestWireGuardOverCarrier is the make-or-break proof: a wireguard-go client and
// server, each on a userspace netstack TUN, exchange real traffic through turnvpn's
// wgbind.Bind over an in-memory datagram carrier (the stand-in for DTLS-over-TURN).
// The client fetches an HTTP page served inside the server's netstack — so the full
// path is HTTP → WG encrypt → carrier → WG decrypt → HTTP, with no UDP socket, no
// TUN device, and no root.
func TestWireGuardOverCarrier(t *testing.T) {
	serverPriv, serverPub := wgKeypair(t)
	clientPriv, clientPub := wgKeypair(t)

	const (
		serverIP = "10.0.0.1"
		clientIP = "10.0.0.2"
	)

	carrierA, carrierB := dgpipe.Pair()

	// --- server side: netstack TUN at 10.0.0.1, WG device over carrierA ---
	srvTun, srvNet, err := netstack.CreateNetTUN(
		[]netip.Addr{netip.MustParseAddr(serverIP)}, nil, 1420)
	if err != nil {
		t.Fatalf("server tun: %v", err)
	}
	srvDev := device.NewDevice(srvTun, wgbind.New(carrierA), device.NewLogger(device.LogLevelError, "wg-server "))
	srvCfg := fmt.Sprintf(""+
		"private_key=%s\n"+
		"listen_port=0\n"+
		"replace_peers=true\n"+
		"public_key=%s\n"+
		"allowed_ip=%s/32\n",
		serverPriv, clientPub, clientIP)
	if err := srvDev.IpcSet(srvCfg); err != nil {
		t.Fatalf("server ipc: %v", err)
	}
	if err := srvDev.Up(); err != nil {
		t.Fatalf("server up: %v", err)
	}
	defer srvDev.Close()

	// HTTP server reachable only through the tunnel, on the server's netstack.
	ln, err := srvNet.ListenTCP(&net.TCPAddr{IP: net.ParseIP(serverIP), Port: 80})
	if err != nil {
		t.Fatalf("server listen: %v", err)
	}
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello-through-wireguard")
	})}
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	// --- client side: netstack TUN at 10.0.0.2, WG device over carrierB ---
	cliTun, cliNet, err := netstack.CreateNetTUN(
		[]netip.Addr{netip.MustParseAddr(clientIP)}, nil, 1420)
	if err != nil {
		t.Fatalf("client tun: %v", err)
	}
	cliDev := device.NewDevice(cliTun, wgbind.New(carrierB), device.NewLogger(device.LogLevelError, "wg-client "))
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

	// Fetch through the client's netstack; the Dial target is the server's tunnel IP.
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return cliNet.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort(serverIP+":80"))
			},
		},
	}

	resp, err := client.Get("http://" + serverIP + "/")
	if err != nil {
		t.Fatalf("GET over wireguard carrier: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != "hello-through-wireguard" {
		t.Fatalf("body = %q, want %q", got, "hello-through-wireguard")
	}
}
