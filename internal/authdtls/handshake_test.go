package authdtls_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/dtls/v3"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
)

// dtlsHandshakeWithIdentity runs a real DTLS handshake over UDP: a server using the
// keyring and a client presenting clientIdentity + clientSecret. It returns the
// client's handshake error (nil on success).
func dtlsHandshakeWithIdentity(t *testing.T, kr authdtls.Keyring, clientIdentity, clientSecret string) error {
	t.Helper()

	laddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := dtls.ListenWithOptions("udp", laddr,
		dtls.WithPSK(kr.PSK()),
		dtls.WithPSKIdentityHint(authdtls.IdentityHint()),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	saddr := ln.Addr().(*net.UDPAddr)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if dc, ok := conn.(*dtls.Conn); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = dc.HandshakeContext(ctx)
			cancel()
		}
		_ = conn.Close()
	}()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	dconn, err := dtls.ClientWithOptions(pc, saddr,
		dtls.WithPSK(authdtls.PSK(clientSecret)),
		dtls.WithPSKIdentityHint([]byte(clientIdentity)),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dconn.HandshakeContext(ctx); err != nil {
		return err
	}
	_ = dconn.Close()
	return nil
}

// TestKeyringHandshakeKnownIdentity: a client whose identity is in the keyring (and
// whose secret matches) completes the handshake.
func TestKeyringHandshakeKnownIdentity(t *testing.T) {
	kr := authdtls.NewKeyring(map[string]string{"alice": "s-alice"})
	if err := dtlsHandshakeWithIdentity(t, kr, "alice", "s-alice"); err != nil {
		t.Fatalf("known identity handshake failed: %v", err)
	}
}

// TestKeyringHandshakeUnknownIdentity: a client whose identity is not in the keyring
// cannot complete the handshake (revocation / unknown client is refused).
func TestKeyringHandshakeUnknownIdentity(t *testing.T) {
	kr := authdtls.NewKeyring(map[string]string{"alice": "s-alice"})
	if err := dtlsHandshakeWithIdentity(t, kr, "mallory", "s-alice"); err == nil {
		t.Fatal("unknown identity completed the handshake (expected failure)")
	}
}
