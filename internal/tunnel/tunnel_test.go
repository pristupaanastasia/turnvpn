package tunnel_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/xtaci/smux"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/mux"
	"github.com/pristupaanastasia/turnvpn/internal/tunnel"
)

const testSecret = "integration-shared-secret"

// echoOrigin starts a TCP server that echoes bytes back — the "website" the client
// reaches through the tunnel. Returns its address and a stop func.
func echoOrigin(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// dialOpts / listenOpts build the PSK-authenticated DTLS options for the test.
func pskClientOpts(secret string) []dtls.ClientOption {
	return []dtls.ClientOption{
		dtls.WithPSK(authdtls.PSK(secret)),
		dtls.WithPSKIdentityHint(authdtls.IdentityHint()),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	}
}

func pskServerOpts(secret string) []dtls.ServerOption {
	return []dtls.ServerOption{
		dtls.WithPSK(authdtls.PSK(secret)),
		dtls.WithPSKIdentityHint(authdtls.IdentityHint()),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	}
}

// buildTunnel wires the full stack over loopback with the given secrets on each end
// and returns the client-side smux session. The server side runs tunnel.Serve.
func buildTunnel(t *testing.T, serverSecret, clientSecret string) (*smux.Session, *mux.Lane) {
	t.Helper()

	laddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := dtls.ListenWithOptions("udp", laddr, pskServerOpts(serverSecret)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	saddr := ln.Addr().(*net.UDPAddr)

	// Server side: accept one DTLS conn, run mux, serve tunnel streams.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if dc, ok := conn.(*dtls.Conn); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := dc.HandshakeContext(ctx)
			cancel()
			if err != nil {
				return
			}
		}
		sess, lane, err := mux.Server(conn)
		if err != nil {
			return
		}
		tunnel.Serve(sess, lane, nil)
	}()

	// Client side: DTLS dial + handshake + mux.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	dconn, err := dtls.ClientWithOptions(pc, saddr, pskClientOpts(clientSecret)...)
	if err != nil {
		t.Fatalf("dtls client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = dconn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		return nil, nil // caller decides whether this is expected (wrong secret)
	}
	t.Cleanup(func() { _ = dconn.Close() })
	csess, clane, err := mux.Client(dconn)
	if err != nil {
		t.Fatalf("mux client: %v", err)
	}
	t.Cleanup(func() { _ = csess.Close() })
	return csess, clane
}

// TestEndToEnd: a byte sent through the whole stack reaches the origin and echoes
// back, proving PSK-DTLS -> KCP -> smux -> target header -> dial -> pipe all work.
func TestEndToEnd(t *testing.T) {
	allowAllTargets(t)
	origin, stop := echoOrigin(t)
	defer stop()

	sess, _ := buildTunnel(t, testSecret, testSecret)
	if sess == nil {
		t.Fatal("tunnel did not establish with matching secrets")
	}

	stream, err := tunnel.Open(sess, origin)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	msg := []byte("hello through PSK-DTLS + KCP + smux + dial")
	if _, err := stream.Write(msg); err != nil {
		t.Fatal(err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

// TestConcurrentStreams: several multiplexed streams over one tunnel each carry
// their own payload independently (exercises smux multiplexing).
func TestConcurrentStreams(t *testing.T) {
	allowAllTargets(t)
	origin, stop := echoOrigin(t)
	defer stop()

	sess, _ := buildTunnel(t, testSecret, testSecret)
	if sess == nil {
		t.Fatal("tunnel did not establish")
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stream, err := tunnel.Open(sess, origin)
			if err != nil {
				errs <- fmt.Errorf("stream %d open: %w", i, err)
				return
			}
			defer func() { _ = stream.Close() }()
			msg := []byte(fmt.Sprintf("payload-%d-%d", i, i*7))
			if _, err := stream.Write(msg); err != nil {
				errs <- err
				return
			}
			_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(stream, got); err != nil {
				errs <- fmt.Errorf("stream %d read: %w", i, err)
				return
			}
			if !bytes.Equal(got, msg) {
				errs <- fmt.Errorf("stream %d echo = %q, want %q", i, got, msg)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// udpEchoOrigin starts a UDP server that echoes each datagram back — the UDP peer
// the client reaches through the relay. Returns its address and a stop func.
func udpEchoOrigin(t *testing.T) (string, func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String(), func() { _ = pc.Close() }
}

// TestUDPRelay: datagrams sent over the unreliable Lane (keyed by flow id, set up by
// a control stream) reach a UDP peer and echo back — OpenUDP -> handleUDP -> UDP dial
// -> lane relay both ways (Stage 7), without riding the reliable stream.
func TestUDPRelay(t *testing.T) {
	allowAllTargets(t)
	origin, stop := udpEchoOrigin(t)
	defer stop()

	sess, lane := buildTunnel(t, testSecret, testSecret)
	if sess == nil {
		t.Fatal("tunnel did not establish")
	}

	const flow = uint32(42)
	got := make(chan []byte, 16)
	lane.SetDatagramHandler(func(f uint32, payload []byte) {
		if f == flow {
			got <- append([]byte(nil), payload...)
		}
	})

	ctrl, err := tunnel.OpenUDP(sess, origin, flow)
	if err != nil {
		t.Fatalf("open udp: %v", err)
	}
	defer func() { _ = ctrl.Close() }()

	// Let the server accept the control stream and register the flow socket.
	time.Sleep(300 * time.Millisecond)

	for _, msg := range [][]byte{
		[]byte("udp-datagram-one"),
		[]byte("second"),
		bytes.Repeat([]byte("x"), 1200),
	} {
		if _, err := lane.SendDatagram(flow, msg); err != nil {
			t.Fatalf("send datagram: %v", err)
		}
		select {
		case echo := <-got:
			if !bytes.Equal(echo, msg) {
				t.Fatalf("udp echo = %q, want %q", echo, msg)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no echo for %q (lane relay)", msg)
		}
	}
}

// TestWrongSecretNoTunnel: a client with the wrong secret cannot establish the
// DTLS session, so no tunnel forms (the exit is not an open proxy).
func TestWrongSecretNoTunnel(t *testing.T) {
	sess, _ := buildTunnel(t, "server-secret", "different-client-secret")
	if sess != nil {
		t.Fatal("tunnel established despite mismatched secrets")
	}
}

// allowAllTargets relaxes the anti-SSRF guard for the duration of a test so the
// loopback echo origins are reachable, then restores the default policy.
func allowAllTargets(t *testing.T) {
	t.Helper()
	orig := tunnel.TargetAllowed
	tunnel.TargetAllowed = func(net.IP) bool { return true }
	t.Cleanup(func() { tunnel.TargetAllowed = orig })
}

// TestTargetAllowedDefault: the default SSRF policy blocks loopback, private,
// link-local (incl. cloud metadata) and unspecified addresses, and permits
// ordinary public addresses.
func TestTargetAllowedDefault(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "169.254.169.254", "10.0.0.1", "192.168.1.1",
		"172.16.0.1", "::1", "fe80::1", "fc00::1", "0.0.0.0",
	}
	for _, s := range blocked {
		if tunnel.TargetAllowed(net.ParseIP(s)) {
			t.Errorf("TargetAllowed(%s) = true, want false (should be blocked)", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "104.16.0.1", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if !tunnel.TargetAllowed(net.ParseIP(s)) {
			t.Errorf("TargetAllowed(%s) = false, want true (public)", s)
		}
	}
}

// TestBlockedTargetRefused: with the default guard, the server refuses to dial a
// loopback target, so no data comes back (SSRF is closed end-to-end).
func TestBlockedTargetRefused(t *testing.T) {
	origin, stop := echoOrigin(t) // on 127.0.0.1 — blocked by the default guard
	defer stop()

	sess, _ := buildTunnel(t, testSecret, testSecret)
	if sess == nil {
		t.Fatal("tunnel did not establish")
	}
	stream, err := tunnel.Open(sess, origin)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	_, _ = stream.Write([]byte("ping"))
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4)
	if n, err := stream.Read(buf); err == nil && n > 0 {
		t.Fatalf("got %q from a blocked loopback target; SSRF guard failed", buf[:n])
	}
}
