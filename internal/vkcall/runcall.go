package vkcall

import (
	"context"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"

	"github.com/pristupaanastasia/turnvpn/internal/vchat"
	"github.com/pristupaanastasia/turnvpn/internal/wttransport"
)

// RunResult reports a completed call attempt.
type RunResult struct {
	ConnectionInfo *vchat.ConnectionInfo
	Connected      bool // whether the PeerConnection reached Connected
}

// RunCall drives a dialed signaling session as the answering WebRTC peer for the
// whole lifetime of ctx: it runs the vchat signaling, answers the SFU's offer
// (pion adds recvonly transceivers for the offered media), trickles ICE, and then
// holds the call — keeping signaling alive (ping/pong, ICE, renegotiation) — until
// ctx is cancelled or signaling ends. Cancel ctx to leave the call.
//
// It owns and tears down everything it creates (the vchat.Conn and the
// PeerConnection); the caller owns the session and closes it. This is the
// native-join capstone; a Vector-2 sender track would be added via a hook before
// Answerer.Run in a later step.
func RunCall(ctx context.Context, sess *wttransport.Session, logf func(string, ...any)) (*RunResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	conn := vchat.NewConn(sess)
	defer conn.Close()

	factory, err := NewPeerFactory()
	if err != nil {
		return nil, err
	}
	pc, answerer, ci, err := Join(ctx, conn, factory)
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	answerer.Logf = logf

	connected := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logf("vkcall: peer connection state: %s", s)
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})

	runErr := make(chan error, 1)
	go func() { runErr <- answerer.Run(ctx) }()

	res := &RunResult{ConnectionInfo: ci}

	// Phase 1: reach Connected (or fail/cancel first).
	select {
	case <-ctx.Done():
		return res, ctx.Err()
	case err := <-runErr:
		if err != nil {
			return res, fmt.Errorf("vkcall: signaling: %w", err)
		}
		return res, fmt.Errorf("vkcall: signaling ended before connect")
	case <-connected:
		res.Connected = true
		logf("vkcall: connected to the call")
	}

	// Phase 2: hold the call (signaling stays alive) until ctx or signaling ends.
	select {
	case <-ctx.Done():
		return res, nil // asked to leave
	case err := <-runErr:
		if err != nil {
			return res, fmt.Errorf("vkcall: signaling dropped: %w", err)
		}
		return res, nil
	}
}
