package vkcall

import (
	"context"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"

	"github.com/pristupaanastasia/turnvpn/internal/vchat"
)

// signaler is the subset of *vchat.Conn the Answerer uses, so it can be faked in
// tests.
type signaler interface {
	Events() <-chan vchat.Message
	SendSDP(participantID int64, sdp vchat.SDP) error
	SendCandidate(participantID int64, c vchat.Candidate) error
}

// peerConn is the subset of *webrtc.PeerConnection the Answerer uses.
type peerConn interface {
	SetRemoteDescription(webrtc.SessionDescription) error
	CreateAnswer(*webrtc.AnswerOptions) (webrtc.SessionDescription, error)
	SetLocalDescription(webrtc.SessionDescription) error
	AddICECandidate(webrtc.ICECandidateInit) error
	OnICECandidate(func(*webrtc.ICECandidate))
}

// The real types must satisfy the interfaces the Answerer is built against.
var (
	_ signaler = (*vchat.Conn)(nil)
	_ peerConn = (*webrtc.PeerConnection)(nil)
)

// Answerer joins a VK Звонки call as the answering WebRTC peer: the SFU offers,
// we answer. It consumes vchat signaling events, drives the pion PeerConnection
// through SetRemoteDescription(offer) → CreateAnswer → SetLocalDescription, sends
// the answer back, and trickles ICE candidates both ways.
//
// Ordering is handled for the SFU's benefit: remote candidates that arrive before
// the offer are queued and applied after SetRemoteDescription; local candidates
// gathered before the answer is sent are buffered and flushed only after it, so
// the SFU never sees a candidate for a peer before that peer's answer.
type Answerer struct {
	sig signaler
	pc  peerConn

	// Logf, if set, receives non-fatal diagnostics (a bad candidate, a trickle
	// send error). nil discards them.
	Logf func(string, ...any)

	mu            sync.Mutex
	peerID        int64             // participant to address; learned from the offer
	answered      bool              // answer SDP sent — local candidates may flow
	localPending  []vchat.Candidate // local candidates gathered before the answer
	remoteSet     bool              // remote description applied
	remotePending []vchat.Candidate // remote candidates that arrived before the offer
}

// NewAnswerer wires the signaler and peer connection and registers local ICE
// trickling. Call Run to drive the exchange.
func NewAnswerer(sig signaler, pc peerConn) *Answerer {
	a := &Answerer{sig: sig, pc: pc}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // end-of-candidates
		}
		cand := toVchatCandidate(c.ToJSON())
		a.mu.Lock()
		if !a.answered {
			a.localPending = append(a.localPending, cand) // flushed after the answer
			a.mu.Unlock()
			return
		}
		pid := a.peerID
		a.mu.Unlock()
		if err := a.sig.SendCandidate(pid, cand); err != nil {
			a.log("vkcall: send local candidate: %v", err)
		}
	})
	return a
}

func (a *Answerer) log(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// Run processes signaling until ctx is cancelled or the event stream ends. It
// returns nil on a clean end and an error only on a fatal signaling/peer failure
// (a bad individual ICE candidate is logged and skipped, not fatal).
func (a *Answerer) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-a.sig.Events():
			if !ok {
				return nil // signaling ended
			}
			if err := a.handle(msg); err != nil {
				return err
			}
		}
	}
}

func (a *Answerer) handle(msg vchat.Message) error {
	sdp, cand, ok := msg.TransmittedData()
	if !ok {
		return nil // not an SDP/ICE message (connection notice, response, …)
	}
	if msg.ParticipantID != 0 {
		a.mu.Lock()
		a.peerID = msg.ParticipantID
		a.mu.Unlock()
	}
	switch {
	case sdp != nil && sdp.Type == "offer":
		return a.answer(*sdp)
	case cand != nil:
		return a.addRemoteCandidate(*cand)
	}
	return nil
}

func (a *Answerer) answer(offer vchat.SDP) error {
	a.mu.Lock()
	pid := a.peerID
	a.mu.Unlock()
	if pid == 0 {
		return fmt.Errorf("vkcall: offer carried no participantId to answer")
	}

	if err := a.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offer.SDP,
	}); err != nil {
		return fmt.Errorf("vkcall: set remote offer: %w", err)
	}
	// Remote description is set: apply any candidates that arrived before it.
	a.mu.Lock()
	a.remoteSet = true
	pendingRemote := a.remotePending
	a.remotePending = nil
	a.mu.Unlock()
	for _, c := range pendingRemote {
		if err := a.pc.AddICECandidate(toICECandidateInit(c)); err != nil {
			a.log("vkcall: add queued remote candidate: %v", err)
		}
	}

	answer, err := a.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("vkcall: create answer: %w", err)
	}
	if err := a.pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("vkcall: set local answer: %w", err)
	}
	if err := a.sig.SendSDP(pid, vchat.SDP{Type: "answer", SDP: answer.SDP}); err != nil {
		return fmt.Errorf("vkcall: send answer: %w", err)
	}
	// Answer is on the wire: flush the local candidates buffered during gathering,
	// then let later ones send directly.
	a.mu.Lock()
	a.answered = true
	flush := a.localPending
	a.localPending = nil
	a.mu.Unlock()
	for _, c := range flush {
		if err := a.sig.SendCandidate(pid, c); err != nil {
			a.log("vkcall: flush local candidate: %v", err)
		}
	}
	return nil
}

func (a *Answerer) addRemoteCandidate(c vchat.Candidate) error {
	a.mu.Lock()
	if !a.remoteSet {
		a.remotePending = append(a.remotePending, c) // applied after SetRemoteDescription
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	// A single bad candidate must not tear down the join.
	if err := a.pc.AddICECandidate(toICECandidateInit(c)); err != nil {
		a.log("vkcall: add remote candidate: %v", err)
	}
	return nil
}

func toICECandidateInit(c vchat.Candidate) webrtc.ICECandidateInit {
	mid := c.SDPMid
	idx := uint16(c.SDPMLineIndex)
	init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: &mid, SDPMLineIndex: &idx}
	if c.UsernameFragment != "" {
		uf := c.UsernameFragment
		init.UsernameFragment = &uf
	}
	return init
}

func toVchatCandidate(init webrtc.ICECandidateInit) vchat.Candidate {
	c := vchat.Candidate{Candidate: init.Candidate}
	if init.SDPMid != nil {
		c.SDPMid = *init.SDPMid
	}
	if init.SDPMLineIndex != nil {
		c.SDPMLineIndex = int(*init.SDPMLineIndex)
	}
	if init.UsernameFragment != nil {
		c.UsernameFragment = *init.UsernameFragment
	}
	return c
}
