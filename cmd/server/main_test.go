package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadPairs(t *testing.T) {
	p := writeFile(t, "pairs", "# comment\n\nalice secretA\nbob secretB\n")
	m, err := readPairs(p, "id secret")
	if err != nil {
		t.Fatalf("readPairs: %v", err)
	}
	if len(m) != 2 || m["alice"] != "secretA" || m["bob"] != "secretB" {
		t.Fatalf("got %v", m)
	}
}

func TestReadPairsErrors(t *testing.T) {
	cases := map[string]string{
		"one field":    "aliceonly\n",
		"three fields": "a b c\n",
		"duplicate":    "a x\na y\n",
	}
	for name, content := range cases {
		if _, err := readPairs(writeFile(t, "f", content), "k v"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := readPairs(filepath.Join(t.TempDir(), "missing"), "k v"); err == nil {
		t.Error("missing file: expected error")
	}
}

func TestLoadKeyring(t *testing.T) {
	kr, err := loadKeyring(writeFile(t, "kr", "alice aaa\nbob bbb\n"))
	if err != nil {
		t.Fatalf("loadKeyring: %v", err)
	}
	if len(kr) != 2 {
		t.Fatalf("keyring size = %d, want 2", len(kr))
	}
	if _, err := loadKeyring(writeFile(t, "empty", "# only comments\n")); err == nil {
		t.Error("empty keyring: expected error")
	}
}

func TestValidWGKey(t *testing.T) {
	priv, _, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := validWGKey(priv); err != nil || got != priv {
		t.Fatalf("validWGKey(valid) = %q, %v", got, err)
	}
	if _, err := validWGKey("nothex"); err == nil {
		t.Error("invalid key accepted")
	}
}

func TestLoadWGKey(t *testing.T) {
	priv, _, _ := wgkey.Generate()

	// From a file.
	if got, err := loadWGKey(writeFile(t, "wg.key", priv+"\n")); err != nil || got != priv {
		t.Fatalf("loadWGKey(file) = %q, %v", got, err)
	}
	// From the environment when no path is given.
	t.Setenv("TURNVPN_WG_KEY", priv)
	if got, err := loadWGKey(""); err != nil || got != priv {
		t.Fatalf("loadWGKey(env) = %q, %v", got, err)
	}
	// Missing everywhere.
	t.Setenv("TURNVPN_WG_KEY", "")
	if _, err := loadWGKey(""); err == nil {
		t.Error("loadWGKey with nothing set: expected error")
	}
	// Invalid file contents.
	if _, err := loadWGKey(writeFile(t, "bad", "nothex")); err == nil {
		t.Error("loadWGKey(bad file): expected error")
	}
}

func TestLoadWGPeers(t *testing.T) {
	_, pub, _ := wgkey.Generate()
	peers, err := loadWGPeers(writeFile(t, "peers", pub+" 10.6.0.2/32\n"))
	if err != nil {
		t.Fatalf("loadWGPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].PubKey != pub || peers[0].AllowedIP != "10.6.0.2/32" {
		t.Fatalf("got %+v", peers)
	}
	// Bad CIDR.
	if _, err := loadWGPeers(writeFile(t, "badcidr", pub+" not-a-cidr\n")); err == nil {
		t.Error("bad allowed-ip: expected error")
	}
	// From env.
	t.Setenv("TURNVPN_WG_PEERS", writeFile(t, "envpeers", pub+" 10.6.0.3/32\n"))
	if peers, err := loadWGPeers(""); err != nil || len(peers) != 1 {
		t.Fatalf("loadWGPeers(env) = %v, %v", peers, err)
	}
	// Missing path and env.
	t.Setenv("TURNVPN_WG_PEERS", "")
	if _, err := loadWGPeers(""); err == nil {
		t.Error("loadWGPeers with nothing set: expected error")
	}
}
