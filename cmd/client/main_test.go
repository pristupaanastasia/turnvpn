package main

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestTurnHostPort(t *testing.T) {
	cases := map[string]string{
		"turn:1.2.3.4:3478?transport=udp": "1.2.3.4:3478",
		"turns:host.example:5349":         "host.example:5349",
		"turn:10.0.0.1:3478":              "10.0.0.1:3478",
		"host:3478":                       "host:3478",
	}
	for in, want := range cases {
		if got := turnHostPort(in); got != want {
			t.Errorf("turnHostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMinDur(t *testing.T) {
	if got := minDur(time.Second, time.Minute); got != time.Second {
		t.Errorf("minDur = %v, want 1s", got)
	}
	if got := minDur(time.Minute, time.Second); got != time.Second {
		t.Errorf("minDur = %v, want 1s", got)
	}
}

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("sleepCtx should return true after elapsing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx should return false when ctx is already cancelled")
	}
}

func TestCloseAllReverseOrder(t *testing.T) {
	var order []int
	mk := func(n int) closerFunc { return func() error { order = append(order, n); return nil } }
	closeAll([]io.Closer{mk(1), mk(2), mk(3)})
	if len(order) != 3 || order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("close order = %v, want [3 2 1]", order)
	}
}

func TestCloserFuncReturnsError(t *testing.T) {
	want := errors.New("boom")
	var f closerFunc = func() error { return want }
	if got := f.Close(); got != want {
		t.Errorf("closerFunc.Close() = %v, want %v", got, want)
	}
}

func TestClientRefTeardownNilSafe(t *testing.T) {
	var r *clientRef
	r.teardown() // must not panic on a nil ref
	// A ref with a nil client and some closers tears the closers down.
	closed := false
	r = &clientRef{closers: []io.Closer{closerFunc(func() error { closed = true; return nil })}}
	r.teardown()
	if !closed {
		t.Error("teardown did not close the carrier closers")
	}
}

func TestTunnelGetNotReady(t *testing.T) {
	var tn tunnel
	if _, err := tn.get(); err == nil {
		t.Error("get() on a fresh tunnel should error (not ready)")
	}
	// setCur with a ref whose client is nil still counts as not ready.
	tn.setCur(&clientRef{})
	if _, err := tn.get(); err == nil {
		t.Error("get() with a nil client should error")
	}
	tn.setCur(nil)
	if _, err := tn.get(); err == nil {
		t.Error("get() after setCur(nil) should error")
	}
}

func TestRunRetriesThenStops(t *testing.T) {
	peer, _ := net.ResolveUDPAddr("udp4", "127.0.0.1:56000")
	tn := &tunnel{
		peers:   []*net.UDPAddr{peer, peer}, // two exits to exercise rotation
		refresh: time.Hour,
		creds: func(ctx context.Context, link string, id int) (string, string, string, error) {
			return "", "", "", errors.New("no creds in test")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(1500 * time.Millisecond); cancel() }()
	// run establishes (fails at creds, before any TURN work), backs off, and
	// returns once the context is cancelled.
	tn.run(ctx)
}
