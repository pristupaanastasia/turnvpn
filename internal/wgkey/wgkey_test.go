package wgkey_test

import (
	"encoding/hex"
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

func TestGenerateShapeAndRoundTrip(t *testing.T) {
	priv, pub, err := wgkey.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// 32-byte keys, hex-encoded.
	for name, k := range map[string]string{"priv": priv, "pub": pub} {
		if len(k) != 64 {
			t.Errorf("%s length = %d, want 64 hex chars", name, len(k))
		}
		if _, err := hex.DecodeString(k); err != nil {
			t.Errorf("%s not valid hex: %v", name, err)
		}
	}
	// Public(priv) must reproduce the public key Generate returned.
	got, err := wgkey.Public(priv)
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	if got != pub {
		t.Fatalf("Public(priv) = %s, want %s", got, pub)
	}
}

func TestGenerateUnique(t *testing.T) {
	a, _, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two Generate calls returned the same private key")
	}
}

func TestPublicDeterministic(t *testing.T) {
	priv, pub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, err := wgkey.Public(priv)
		if err != nil {
			t.Fatal(err)
		}
		if got != pub {
			t.Fatalf("Public call %d = %s, want %s", i, got, pub)
		}
	}
}

func TestPublicErrors(t *testing.T) {
	cases := map[string]string{
		"not hex":    "nothexnothex",
		"too short":  "00112233",
		"odd length": "abc",
		"33 bytes":   "00" + "11223344556677889900112233445566778899001122334455667788990011223344",
	}
	for name, in := range cases {
		if _, err := wgkey.Public(in); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
