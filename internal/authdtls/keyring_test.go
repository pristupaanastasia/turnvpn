package authdtls_test

import (
	"bytes"
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
)

// TestKeyring: the server keyring returns each client's key by identity, rejects an
// unknown identity, and rejects a revoked one.
func TestKeyring(t *testing.T) {
	kr := authdtls.NewKeyring(map[string]string{"alice": "s-alice", "bob": "s-bob"})
	cb := kr.PSK()

	k, err := cb([]byte("alice"))
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	if !bytes.Equal(k, authdtls.Key("s-alice")) {
		t.Fatal("alice key != Key(secret)")
	}

	if _, err := cb([]byte("carol")); err == nil {
		t.Fatal("unknown identity was accepted")
	}

	// Revoke bob: the handshake for bob must now fail.
	delete(kr, "bob")
	if _, err := cb([]byte("bob")); err == nil {
		t.Fatal("revoked identity was accepted")
	}
}

// TestKey: the derivation is deterministic, domain-separated and 32 bytes.
func TestKey(t *testing.T) {
	if !bytes.Equal(authdtls.Key("x"), authdtls.Key("x")) {
		t.Fatal("Key not deterministic")
	}
	if bytes.Equal(authdtls.Key("x"), authdtls.Key("y")) {
		t.Fatal("different secrets derive the same key")
	}
	if n := len(authdtls.Key("x")); n != 32 {
		t.Fatalf("key length = %d, want 32", n)
	}
}
