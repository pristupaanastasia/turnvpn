package vkcall

import (
	"context"
	"errors"
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
// (pion adds recvonly transceivers for the offered media), trickles ICE, waits for
// the PeerConnection to reach Connected, then holds the call — keeping signaling
// alive (ping/pong, ICE, renegotiation) — until ctx is cancelled or the peer/
// signaling drops. Cancel ctx to leave.
//
// On exit it tears down everything it uses: the vchat.Conn and PeerConnection it
// creates, and the session (Close is idempotent, so the caller's own session Close
// is a safe no-op; closing the session first also unblocks any stuck signaling
// write so shutdown can't hang). It returns a distinct error if it never connects,
// so a never-connected attempt is not mistaken for success.
func RunCall(ctx context.Context, sess *wttransport.Session, logf func(string, ...any)) (*RunResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	conn := vchat.NewConn(sess)

	factory, err := NewPeerFactory()
	if err != nil {
		conn.Close()
		return nil, err
	}
	pc, answerer, ci, err := Join(ctx, conn, factory)
	if err != nil {
		conn.Close()
		return nil, err
	}
	answerer.Logf = logf

	connected := make(chan struct{})
	failed := make(chan struct{})
	var connOnce, failOnce sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logf("vkcall: peer connection state: %s", s)
		switch s {
		case webrtc.PeerConnectionStateConnected:
			connOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			failOnce.Do(func() { close(failed) })
		}
	})

	runErr := make(chan error, 1)
	go func() { runErr <- answerer.Run(ctx) }()

	runDone := false
	finish := func() {
		sess.Close() // idempotent; unblock any stuck signaling write first
		conn.Close() // stop the vchat loop -> Answerer.Run's Events closes
		if !runDone {
			<-runErr // join the Answerer.Run goroutine
		}
		pc.Close()
	}

	res := &RunResult{ConnectionInfo: ci}

	// Phase 1: reach Connected (or fail / drop / time out first).
	select {
	case <-ctx.Done():
		finish()
		return res, fmt.Errorf("vkcall: timed out before connect: %w", ctx.Err())
	case err := <-runErr:
		runDone = true
		finish()
		if err != nil {
			return res, fmt.Errorf("vkcall: signaling: %w", err)
		}
		return res, errors.New("vkcall: signaling ended before connect")
	case <-failed:
		finish()
		return res, errors.New("vkcall: peer connection failed before connect")
	case <-connected:
		res.Connected = true
		logf("vkcall: connected to the call")
	}

	// Phase 2: hold the call until ctx is cancelled or the peer/signaling drops.
	select {
	case <-ctx.Done():
		finish()
		return res, nil // asked to leave
	case err := <-runErr:
		runDone = true
		finish()
		if err != nil {
			return res, fmt.Errorf("vkcall: signaling dropped: %w", err)
		}
		return res, nil
	case <-failed:
		finish()
		res.Connected = false
		return res, errors.New("vkcall: peer connection dropped")
	}
}
