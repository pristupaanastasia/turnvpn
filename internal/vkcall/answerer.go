package vkcall

import (
	"context"
	"fmt"
	"sync/atomic"

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
type Answerer struct {
	sig    signaler
	pc     peerConn
	peerID atomic.Int64 // the participant to address (learned from the offer)
}

// NewAnswerer wires the signaler and peer connection and registers local ICE
// trickling. Call Run to drive the exchange.
func NewAnswerer(sig signaler, pc peerConn) *Answerer {
	a := &Answerer{sig: sig, pc: pc}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // end-of-candidates
		}
		pid := a.peerID.Load()
		if pid == 0 {
			return // no peer addressed yet; the offer sets it before gathering
		}
		_ = a.sig.SendCandidate(pid, toVchatCandidate(c.ToJSON()))
	})
	return a
}

// Run processes signaling until an offer has been answered, then keeps trickling
// candidates until ctx is cancelled or the event stream ends. It returns nil on a
// clean end (ctx done or events closed) and an error only on a signaling failure.
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
		a.peerID.Store(msg.ParticipantID)
	}
	switch {
	case sdp != nil && sdp.Type == "offer":
		return a.answer(*sdp)
	case cand != nil:
		return a.addCandidate(*cand)
	}
	return nil
}

func (a *Answerer) answer(offer vchat.SDP) error {
	if err := a.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offer.SDP,
	}); err != nil {
		return fmt.Errorf("vkcall: set remote offer: %w", err)
	}
	answer, err := a.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("vkcall: create answer: %w", err)
	}
	if err := a.pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("vkcall: set local answer: %w", err)
	}
	if err := a.sig.SendSDP(a.peerID.Load(), vchat.SDP{Type: "answer", SDP: answer.SDP}); err != nil {
		return fmt.Errorf("vkcall: send answer: %w", err)
	}
	return nil
}

func (a *Answerer) addCandidate(c vchat.Candidate) error {
	if err := a.pc.AddICECandidate(toICECandidateInit(c)); err != nil {
		return fmt.Errorf("vkcall: add remote candidate: %w", err)
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
