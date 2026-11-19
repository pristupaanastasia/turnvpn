package mux

import (
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
)

func TestApplyLaneOptsWithRate(t *testing.T) {
	a, b := dgpipe.Pair()
	defer a.Close()
	defer b.Close()
	lane := NewLane(a)
	defer lane.Close()
	applyLaneOpts(lane, []Option{WithRate(100000)})
	if lane.shaper == nil {
		t.Fatal("WithRate did not install a shaper")
	}
	// Zero rate leaves shaping off.
	lane2 := NewLane(b)
	defer lane2.Close()
	applyLaneOpts(lane2, []Option{WithRate(0)})
	if lane2.shaper != nil {
		t.Error("rate 0 should not install a shaper")
	}
}

func TestRelConnMeta(t *testing.T) {
	a, b := dgpipe.Pair()
	defer a.Close()
	defer b.Close()
	lane := NewLane(a)
	pc := lane.PacketConn()
	if pc.LocalAddr() == nil {
		t.Error("LocalAddr nil")
	}
	if err := pc.SetDeadline(time.Now()); err != nil {
		t.Error(err)
	}
	if err := pc.SetReadDeadline(time.Now()); err != nil {
		t.Error(err)
	}
	if err := pc.SetWriteDeadline(time.Now()); err != nil {
		t.Error(err)
	}
	if err := pc.Close(); err != nil {
		t.Error(err)
	}
}

func TestShapedLaneSend(t *testing.T) {
	a, b := dgpipe.Pair()
	defer a.Close()
	defer b.Close()
	lane := NewLane(a)
	defer lane.Close()
	applyLaneOpts(lane, []Option{WithRate(1000000)})
	// SendDatagram goes through writeTagged, which paces via the shaper.
	if _, err := lane.SendDatagram(1, []byte("hello")); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
}
