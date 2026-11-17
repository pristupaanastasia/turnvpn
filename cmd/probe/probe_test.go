package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v4"
)

// TestRoundTripLocal proves the probe's core mechanic end to end, without VK and
// without the remote VPS: it stands up a conformant pion TURN server and a UDP
// echo peer on loopback, allocates a relay through the real turn.Client, and
// drives the real roundTrip. A pass means allocate -> permission -> relay out ->
// echo -> relay back all work in our code; so a TIMEOUT against a real service
// is that service's relay policy, not a bug here.
func TestRoundTripLocal(t *testing.T) {
	const (
		user  = "probe"
		pass  = "probe-secret"
		realm = "turnvpn.local"
	)

	echoAddr := startEchoPeer(t)

	// TURN server on loopback. Relays are created on 127.0.0.1 so the server
	// can actually reach the echo peer, which also lives on loopback.
	serverPC, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen turn server: %v", err)
	}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(u, r string, _ net.Addr) ([]byte, bool) {
			if u == user && r == realm {
				return turn.GenerateAuthKey(u, r, pass), true
			}
			return nil, false
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: serverPC,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP("127.0.0.1"),
				Address:      "127.0.0.1",
			},
		}},
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		t.Fatalf("new turn server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	serverAddr := serverPC.LocalAddr().String()

	// Real turn.Client, same as the probe uses.
	clientPC, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen client: %v", err)
	}
	t.Cleanup(func() { _ = clientPC.Close() })

	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: serverAddr,
		TURNServerAddr: serverAddr,
		Conn:           clientPC,
		Username:       user,
		Password:       pass,
		Realm:          realm,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		t.Fatalf("new turn client: %v", err)
	}
	t.Cleanup(client.Close)

	if err := client.Listen(); err != nil {
		t.Fatalf("client listen: %v", err)
	}

	relayConn, err := client.Allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	t.Cleanup(func() { _ = relayConn.Close() })

	timeout := 8 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := roundTrip(ctx, relayConn, echoAddr, timeout); err != nil {
		t.Fatalf("roundTrip through local relay failed: %v", err)
	}
}

// startEchoPeer runs a loopback UDP echo (the local stand-in for vpsecho) and
// returns its address. It is torn down when the test ends.
func startEchoPeer(t *testing.T) *net.UDPAddr {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo peer: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 1600)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return // closed on cleanup
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()

	return pc.LocalAddr().(*net.UDPAddr)
}
