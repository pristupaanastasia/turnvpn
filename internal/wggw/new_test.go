package wggw_test

import (
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

func TestNewErrors(t *testing.T) {
	priv, _, _ := wgkey.Generate()
	_, pub, _ := wgkey.Generate()

	a, _ := dgpipe.Pair()
	defer a.Close()
	if _, err := wggw.New(a, wggw.Config{ServerPrivKey: priv, Peers: nil}); err == nil {
		t.Error("no peers should error")
	}

	b, _ := dgpipe.Pair()
	defer b.Close()
	if _, err := wggw.New(b, wggw.Config{ServerPrivKey: "nothex", Peers: []wggw.Peer{{PubKey: pub, AllowedIP: "10.6.0.2/32"}}}); err == nil {
		t.Error("bad server key should error")
	}
}
