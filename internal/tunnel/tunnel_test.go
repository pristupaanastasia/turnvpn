package tunnel_test

import (
	"bytes"
	"context"
	"encoding/binary"
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
func buildTunnel(t *testing.T, serverSecret, clientSecret string) *smux.Session {
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
		sess, err := mux.Server(conn)
		if err != nil {
			return
		}
		tunnel.Serve(sess, nil)
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
		return nil // caller decides whether this is expected (wrong secret)
	}
	t.Cleanup(func() { _ = dconn.Close() })
	csess, err := mux.Client(dconn)
	if err != nil {
		t.Fatalf("mux client: %v", err)
	}
	t.Cleanup(func() { _ = csess.Close() })
	return csess
}

// TestEndToEnd: a byte sent through the whole stack reaches the origin and echoes
// back, proving PSK-DTLS -> KCP -> smux -> target header -> dial -> pipe all work.
func TestEndToEnd(t *testing.T) {
	origin, stop := echoOrigin(t)
	defer stop()

	sess := buildTunnel(t, testSecret, testSecret)
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
	origin, stop := echoOrigin(t)
	defer stop()

	sess := buildTunnel(t, testSecret, testSecret)
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

// TestUDPRelay: datagrams framed over a tunnel stream reach a UDP peer and echo
// back, proving OpenUDP -> handleUDP -> UDP dial -> relay both ways (Stage 7).
func TestUDPRelay(t *testing.T) {
	origin, stop := udpEchoOrigin(t)
	defer stop()

	sess := buildTunnel(t, testSecret, testSecret)
	if sess == nil {
		t.Fatal("tunnel did not establish")
	}

	stream, err := tunnel.OpenUDP(sess, origin)
	if err != nil {
		t.Fatalf("open udp: %v", err)
	}
	defer func() { _ = stream.Close() }()

	send := func(payload []byte) {
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(payload)))
		if _, err := stream.Write(append(lb[:], payload...)); err != nil {
			t.Fatalf("write datagram: %v", err)
		}
	}
	recv := func() []byte {
		_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
		var lb [2]byte
		if _, err := io.ReadFull(stream, lb[:]); err != nil {
			t.Fatalf("read datagram len: %v", err)
		}
		b := make([]byte, int(binary.BigEndian.Uint16(lb[:])))
		if _, err := io.ReadFull(stream, b); err != nil {
			t.Fatalf("read datagram: %v", err)
		}
		return b
	}

	// Several datagrams over one association, including a larger one.
	for _, msg := range [][]byte{
		[]byte("udp-datagram-one"),
		[]byte("second"),
		bytes.Repeat([]byte("x"), 1200),
	} {
		send(msg)
		if got := recv(); !bytes.Equal(got, msg) {
			t.Fatalf("udp echo = %q, want %q", got, msg)
		}
	}
}

// TestWrongSecretNoTunnel: a client with the wrong secret cannot establish the
// DTLS session, so no tunnel forms (the exit is not an open proxy).
func TestWrongSecretNoTunnel(t *testing.T) {
	sess := buildTunnel(t, "server-secret", "different-client-secret")
	if sess != nil {
		t.Fatal("tunnel established despite mismatched secrets")
	}
}
