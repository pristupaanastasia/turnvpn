package vkcall

import (
	"context"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/pristupaanastasia/turnvpn/internal/vchat"
)

type sentSDP struct {
	pid int64
	sdp vchat.SDP
}
type sentCand struct {
	pid  int64
	cand vchat.Candidate
}

type fakeSignaler struct {
	events chan vchat.Message
	sdps   []sentSDP
	cands  []sentCand
}

func newFakeSignaler() *fakeSignaler {
	return &fakeSignaler{events: make(chan vchat.Message, 16)}
}
func (f *fakeSignaler) Events() <-chan vchat.Message { return f.events }
func (f *fakeSignaler) SendSDP(pid int64, sdp vchat.SDP) error {
	f.sdps = append(f.sdps, sentSDP{pid, sdp})
	return nil
}
func (f *fakeSignaler) SendCandidate(pid int64, c vchat.Candidate) error {
	f.cands = append(f.cands, sentCand{pid, c})
	return nil
}

type fakePC struct {
	remote    *webrtc.SessionDescription
	local     *webrtc.SessionDescription
	added     []webrtc.ICECandidateInit
	onICE     func(*webrtc.ICECandidate)
	answerSDP string
}

func (p *fakePC) SetRemoteDescription(d webrtc.SessionDescription) error {
	p.remote = &d
	return nil
}
func (p *fakePC) CreateAnswer(*webrtc.AnswerOptions) (webrtc.SessionDescription, error) {
	return webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: p.answerSDP}, nil
}
func (p *fakePC) SetLocalDescription(d webrtc.SessionDescription) error {
	p.local = &d
	return nil
}
func (p *fakePC) AddICECandidate(c webrtc.ICECandidateInit) error {
	p.added = append(p.added, c)
	return nil
}
func (p *fakePC) OnICECandidate(f func(*webrtc.ICECandidate)) { p.onICE = f }

func msgOffer(pid int64, sdp string) vchat.Message {
	m, _ := vchat.Parse([]byte(`{"type":"notification","notification":"transmitted-data","participantId":` +
		itoa(pid) + `,"data":{"sdp":{"type":"offer","sdp":"` + sdp + `"}}}`))
	return m
}
func msgCandidate(cand string) vchat.Message {
	m, _ := vchat.Parse([]byte(`{"type":"notification","notification":"transmitted-data","data":{"candidate":{"candidate":"` +
		cand + `","sdpMid":"0","sdpMLineIndex":0}}}`))
	return m
}
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func TestAnswererAnswersOffer(t *testing.T) {
	sig := newFakeSignaler()
	pc := &fakePC{answerSDP: "ANSWER"}
	a := NewAnswerer(sig, pc)

	sig.events <- msgOffer(99, "OFFER")
	close(sig.events) // Run returns when events end
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pc.remote == nil || pc.remote.Type != webrtc.SDPTypeOffer || pc.remote.SDP != "OFFER" {
		t.Errorf("remote description: %+v", pc.remote)
	}
	if pc.local == nil || pc.local.Type != webrtc.SDPTypeAnswer {
		t.Errorf("local description: %+v", pc.local)
	}
	if len(sig.sdps) != 1 || sig.sdps[0].pid != 99 || sig.sdps[0].sdp.Type != "answer" || sig.sdps[0].sdp.SDP != "ANSWER" {
		t.Errorf("sent answer: %+v", sig.sdps)
	}
}

func TestAnswererRemoteCandidate(t *testing.T) {
	sig := newFakeSignaler()
	pc := &fakePC{answerSDP: "A"}
	a := NewAnswerer(sig, pc)

	sig.events <- msgCandidate("candidate:1 1 udp 2 1.2.3.4 5 typ host")
	close(sig.events)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pc.added) != 1 || pc.added[0].Candidate != "candidate:1 1 udp 2 1.2.3.4 5 typ host" {
		t.Errorf("added candidate: %+v", pc.added)
	}
	if pc.added[0].SDPMid == nil || *pc.added[0].SDPMid != "0" {
		t.Errorf("sdpMid: %+v", pc.added[0].SDPMid)
	}
}

func TestAnswererLocalICETrickle(t *testing.T) {
	sig := newFakeSignaler()
	pc := &fakePC{answerSDP: "A"}
	a := NewAnswerer(sig, pc)

	// Before any offer, peerID is 0: local candidates are not sent.
	pc.onICE(&webrtc.ICECandidate{Foundation: "x"})
	if len(sig.cands) != 0 {
		t.Errorf("sent candidate before peer known: %+v", sig.cands)
	}
	// After the offer sets the peer, local candidates are trickled to it.
	sig.events <- msgOffer(77, "O")
	close(sig.events)
	a.Run(context.Background())
	pc.onICE(&webrtc.ICECandidate{Foundation: "y"})
	if len(sig.cands) != 1 || sig.cands[0].pid != 77 {
		t.Errorf("trickled candidate: %+v", sig.cands)
	}
	// A nil candidate (end of gathering) is not sent.
	pc.onICE(nil)
	if len(sig.cands) != 1 {
		t.Errorf("nil candidate should not be sent: %+v", sig.cands)
	}
}

func TestCandidateConversionRoundTrip(t *testing.T) {
	in := vchat.Candidate{Candidate: "candidate:1", SDPMid: "0", SDPMLineIndex: 2, UsernameFragment: "uf"}
	got := toVchatCandidate(toICECandidateInit(in))
	if got != in {
		t.Errorf("round trip: got %+v want %+v", got, in)
	}
}

func TestAnswererContextCancel(t *testing.T) {
	sig := newFakeSignaler()
	a := NewAnswerer(sig, &fakePC{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Errorf("cancelled Run should return nil, got %v", err)
	}
}
