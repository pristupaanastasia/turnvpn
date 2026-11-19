package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/turn/v5"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

// TestEstablishEndToEnd drives the real establish(): a local pion/turn server
// relays the client's DTLS to a loopback DTLS peer running the WireGuard gateway,
// and the resulting tunnel proxies an HTTP request. This is the full client
// assembly (TURN allocate + PSK-DTLS + WireGuard) minus the VK credential fetch.
func TestEstablishEndToEnd(t *testing.T) {
	const psk = "establish-test-secret"

	// Origin reachable through the gateway (mapped from a routed test address).
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	go func() {
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				_, _ = c.Read(buf)
				_, _ = io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nhi")
				_ = c.Close()
			}(c)
		}
	}()

	serverPriv, _, _ := wgkey.Generate()
	serverPub, _ := wgkey.Public(serverPriv)
	clientPriv, clientPub, _ := wgkey.Generate()
	const clientIP = "10.6.0.2"

	// DTLS peer running the WireGuard gateway (the "VPS exit").
	pskOpts := []dtls.ServerOption{
		dtls.WithPSK(authdtls.PSK(psk)),
		dtls.WithPSKIdentityHint(authdtls.IdentityHint()),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	}
	dtlsLn, err := dtls.ListenWithOptions("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, pskOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer dtlsLn.Close()
	peerAddr := dtlsLn.Addr().(*net.UDPAddr)
	go func() {
		conn, err := dtlsLn.Accept()
		if err != nil {
			return
		}
		_, _ = wggw.New(conn, wggw.Config{
			ServerPrivKey: serverPriv,
			Peers:         []wggw.Peer{{PubKey: clientPub, AllowedIP: clientIP + "/32"}},
			MTU:           1280,
			Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Addr().String())
			},
		})
	}()

	// Local TURN server.
	turnConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: "pion.ly",
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			return ra.Username, turn.GenerateAuthKey(ra.Username, ra.Realm, "turnpass"), true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: turnConn,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP("127.0.0.1"),
				Address:      "0.0.0.0",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	turnAddr := turnConn.LocalAddr().String()

	tn := &tunnel{
		cfg: config{
			secret: psk, wgPriv: clientPriv, wgServerPub: serverPub, wgAddr: clientIP, mtu: 1280,
		},
		peers: []*net.UDPAddr{peerAddr},
		creds: func(context.Context, string, int) (string, string, string, error) {
			return "turnuser", "turnpass", turnAddr, nil
		},
	}

	ref, err := tn.establish(context.Background())
	if err != nil {
		t.Fatalf("establish: %v", err)
	}
	defer ref.teardown()

	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: ref.client.DialContext}}
	resp, err := client.Get("http://192.0.2.99:80/")
	if err != nil {
		t.Fatalf("GET through established tunnel: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hi" {
		t.Fatalf("body = %q, want hi", body)
	}
}
