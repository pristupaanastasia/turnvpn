package turnvpn

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestHello(t *testing.T) {
	if Hello() == "" {
		t.Error("Hello returned empty")
	}
}

func TestGenerateWGKeypair(t *testing.T) {
	pair, err := GenerateWGKeypair()
	if err != nil {
		t.Fatalf("GenerateWGKeypair: %v", err)
	}
	fields := strings.Fields(pair)
	if len(fields) != 2 {
		t.Fatalf("expected 'priv pub', got %q", pair)
	}
	for _, k := range fields {
		if b, err := hex.DecodeString(k); err != nil || len(b) != 32 {
			t.Errorf("not a 32-byte hex key: %q", k)
		}
	}
}

func TestStartRejectsBadPeer(t *testing.T) {
	// A malformed peer address fails fast, before any carrier work; also exercises
	// the makeTUN stub path on non-Android hosts.
	err := Start(0, 1280, "not a host:port", "secret", "link", "token", "appkey", "aa", "bb", "10.6.0.2", "")
	if err == nil {
		t.Error("Start with a bad peer should error")
	}
}
