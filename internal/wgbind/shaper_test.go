package wgbind

import (
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
)

// TestShaperPaces: once the burst is drained, the shaper blocks so the average rate
// is held (it cannot hand out a second burst immediately).
func TestShaperPaces(t *testing.T) {
	s := newShaper(100000) // 100 KB/s, burst ~100 KB
	s.take(100000)         // drain the burst
	start := time.Now()
	s.take(50000) // needs ~0.5s to refill at 100 KB/s
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("shaper did not pace: took %v, expected ~0.5s", d)
	}
}

func TestParseBitrate(t *testing.T) {
	for in, want := range map[string]int{
		"2mbit":   250000,
		"256kbit": 32000,
		"500kbps": 62500,
		"1000000": 125000, // bare = bits/sec
		"":        0,
	} {
		got, err := ParseBitrate(in)
		if err != nil {
			t.Fatalf("ParseBitrate(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseBitrate(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := ParseBitrate("abc"); err == nil {
		t.Error("bad bitrate accepted")
	}
}

func TestNewWithRateInstallsShaper(t *testing.T) {
	a, b := dgpipe.Pair()
	defer a.Close()
	defer b.Close()
	if NewWithRate(a, 0).shaper != nil {
		t.Error("rate 0 should not install a shaper")
	}
	if NewWithRate(b, 100000).shaper == nil {
		t.Error("positive rate should install a shaper")
	}
}
