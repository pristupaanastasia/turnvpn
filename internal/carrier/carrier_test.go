package carrier

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/turn/v5"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
)

func TestTurnHostPort(t *testing.T) {
	cases := map[string]string{
		"turn:1.2.3.4:3478?transport=udp": "1.2.3.4:3478",
		"turns:host.example:5349":         "host.example:5349",
		"host:3478":                       "host:3478",
	}
	for in, want := range cases {
		if got := turnHostPort(in); got != want {
			t.Errorf("turnHostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTokenCredsNotNil(t *testing.T) {
	if TokenCreds("app", "link", "tok") == nil {
		t.Fatal("TokenCreds returned nil")
	}
}

// TestDial brings the carrier up over a local pion/turn server to a loopback DTLS
// peer, the same path the real client takes minus the VK credential fetch.
func TestDial(t *testing.T) {
	const psk = "carrier-test-secret"

	// DTLS peer (stands in for the VPS exit): accept and hold the connection.
	pskOpts := []dtls.ServerOption{
		dtls.WithPSK(authdtls.PSK(psk)),
		dtls.WithPSKIdentityHint(authdtls.IdentityHint()),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	}
	ln, err := dtls.ListenWithOptions("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, pskOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peerAddr := ln.Addr().(*net.UDPAddr)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Drive the server side of the handshake and hold the conn open.
		buf := make([]byte, 1500)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, closers, err := Dial(ctx, Config{
		Peer:   peerAddr,
		Secret: psk,
		Creds: func(context.Context) (string, string, string, error) {
			return "turnuser", "turnpass", turnAddr, nil
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if conn == nil {
		t.Fatal("nil carrier conn")
	}
	closeAll(closers)
}

func TestDialCredsError(t *testing.T) {
	_, _, err := Dial(context.Background(), Config{
		Peer:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		Secret: "x",
		Creds: func(context.Context) (string, string, string, error) {
			return "", "", "", context.Canceled
		},
	})
	if err == nil {
		t.Fatal("expected creds error")
	}
}
