package vkcall

import (
	"context"
	"errors"
	"fmt"

	"github.com/pion/webrtc/v4"

	"github.com/pristupaanastasia/turnvpn/internal/vchat"
)

// awaitConnection reads signaling events until the "connection" notification and
// returns its ConnectionInfo (the ICE servers and conversation identity). Earlier
// events are ignored. It errors if the stream ends first or ctx is cancelled.
func awaitConnection(ctx context.Context, events <-chan vchat.Message) (*vchat.ConnectionInfo, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg, ok := <-events:
			if !ok {
				return nil, errors.New("vkcall: signaling ended before the connection notification")
			}
			if ci, ok := msg.Connection(); ok {
				return ci, nil
			}
		}
	}
}

// PeerFactory builds a pion PeerConnection for the given configuration, so the
// caller controls the MediaEngine and codecs (e.g. a VP8-only sender for Vector
// 2). webrtc.NewPeerConnection is a valid PeerFactory for a default engine.
type PeerFactory func(webrtc.Configuration) (*webrtc.PeerConnection, error)

// Join sets up the answering peer for a dialed signaling connection: it waits for
// the connection notification, builds the PeerConnection with the SFU's ICE
// servers, and wires an Answerer. It does NOT start the exchange or add media —
// the caller adds its tracks/transceivers to the returned PeerConnection and then
// calls Answerer.Run to drive the offer/answer/ICE handshake. The caller owns and
// closes the PeerConnection, the Answerer, the signaling Conn and the transport.
//
// Keep the window between Join returning and Answerer.Run short: until Run starts,
// inbound events fill vchat.Conn's bounded Events buffer, and once it is full the
// read loop can no longer answer heartbeat pings, which may make the SFU drop the
// session. Add tracks quickly, then call Run (e.g. in a goroutine).
func Join(ctx context.Context, sig *vchat.Conn, newPeer PeerFactory) (*webrtc.PeerConnection, *Answerer, *vchat.ConnectionInfo, error) {
	if sig == nil || newPeer == nil {
		return nil, nil, nil, errors.New("vkcall: Join needs a non-nil signaling conn and peer factory")
	}
	ci, err := awaitConnection(ctx, sig.Events())
	if err != nil {
		return nil, nil, nil, err
	}
	pc, err := newPeer(Configuration(ci))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("vkcall: create peer: %w", err)
	}
	if pc == nil {
		return nil, nil, nil, errors.New("vkcall: peer factory returned a nil PeerConnection")
	}
	return pc, NewAnswerer(sig, pc), ci, nil
}
