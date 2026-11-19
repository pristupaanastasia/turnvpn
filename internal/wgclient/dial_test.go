package wgclient_test

import (
	"context"
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/wgclient"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

func TestNewBadAddress(t *testing.T) {
	a, _ := dgpipe.Pair()
	defer a.Close()
	priv, _, _ := wgkey.Generate()
	_, pub, _ := wgkey.Generate()
	if _, err := wgclient.New(a, wgclient.Config{PrivKey: priv, ServerPub: pub, Address: "not-an-ip"}); err == nil {
		t.Fatal("expected error for a bad tunnel address")
	}
}

func TestDialContextInputErrors(t *testing.T) {
	a, b := dgpipe.Pair()
	defer a.Close()
	defer b.Close()
	priv, _, _ := wgkey.Generate()
	_, pub, _ := wgkey.Generate()
	c, err := wgclient.New(a, wgclient.Config{PrivKey: priv, ServerPub: pub, Address: "10.6.0.2"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	// These all fail while parsing the address, before any network I/O.
	cases := map[string]struct{ net, addr string }{
		"no port":         {"tcp", "example.com"},
		"bad port":        {"tcp", "example.com:notaport"},
		"unsupported net": {"sctp", "1.1.1.1:80"},
	}
	for name, c2 := range cases {
		if _, err := c.DialContext(ctx, c2.net, c2.addr); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
