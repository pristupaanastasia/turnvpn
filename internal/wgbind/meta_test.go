package wgbind_test

import (
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
)

func TestBindMeta(t *testing.T) {
	a, _ := dgpipe.Pair()
	defer a.Close()
	b := wgbind.New(a)
	if b.BatchSize() != 1 {
		t.Errorf("BatchSize = %d, want 1", b.BatchSize())
	}
	if err := b.SetMark(7); err != nil {
		t.Errorf("SetMark: %v", err)
	}
	ep, err := b.ParseEndpoint("ignored")
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	ep.ClearSrc()
	_ = ep.SrcToString()
	if ep.DstToString() == "" {
		t.Error("DstToString empty")
	}
	_ = ep.DstToBytes()
	_ = ep.DstIP()
	_ = ep.SrcIP()
}
