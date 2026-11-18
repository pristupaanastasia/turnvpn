package authdtls

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
)

func listen(t *testing.T, secret string) (net.Listener, *net.UDPAddr) {
	t.Helper()
	laddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := dtls.ListenWithOptions("udp", laddr,
		dtls.WithPSK(PSK(secret)),
		dtls.WithPSKIdentityHint(IdentityHint()),
		dtls.WithCipherSuites(Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	)
	if err != nil {
		t.Fatal(err)
	}
	return ln, ln.Addr().(*net.UDPAddr)
}

func dialHandshake(t *testing.T, addr *net.UDPAddr, secret string) (*dtls.Conn, error) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dtls.ClientWithOptions(pc, addr,
		dtls.WithPSK(PSK(secret)),
		dtls.WithPSKIdentityHint(IdentityHint()),
		dtls.WithCipherSuites(Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// TestPSKHandshakeAndEcho: matching secrets complete the handshake and carry data.
func TestPSKHandshakeAndEcho(t *testing.T) {
	ln, addr := listen(t, "s3cret")
	defer func() { _ = ln.Close() }()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer func() { _ = conn.Close() }()
		if dc, ok := conn.(*dtls.Conn); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := dc.HandshakeContext(ctx)
			cancel()
			if err != nil {
				srvErr <- err
				return
			}
		}
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			srvErr <- err
			return
		}
		_, err = conn.Write(buf[:n])
		srvErr <- err
	}()

	conn, err := dialHandshake(t, addr, "s3cret")
	if err != nil {
		t.Fatalf("handshake with matching secret: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("echo = %q, want ping", buf[:n])
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

// TestPSKWrongSecretFails: a mismatched secret must not complete the handshake.
func TestPSKWrongSecretFails(t *testing.T) {
	ln, addr := listen(t, "right-secret")
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if dc, ok := conn.(*dtls.Conn); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = dc.HandshakeContext(ctx) // expected to fail
			cancel()
		}
		_ = conn.Close()
	}()

	if _, err := dialHandshake(t, addr, "wrong-secret"); err == nil {
		t.Fatal("handshake succeeded with a wrong secret; want failure")
	}
}
