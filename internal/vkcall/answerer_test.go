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
	addErr    error
}

func (p *fakePC) SetRemoteDescription(d webrtc.SessionDescription) error { p.remote = &d; return nil }
func (p *fakePC) CreateAnswer(*webrtc.AnswerOptions) (webrtc.SessionDescription, error) {
	return webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: p.answerSDP}, nil
}
func (p *fakePC) SetLocalDescription(d webrtc.SessionDescription) error { p.local = &d; return nil }
func (p *fakePC) AddICECandidate(c webrtc.ICECandidateInit) error {
	if p.addErr != nil {
		return p.addErr
	}
	p.added = append(p.added, c)
	return nil
}
func (p *fakePC) OnICECandidate(f func(*webrtc.ICECandidate)) { p.onICE = f }

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

func TestAnswererAnswersOffer(t *testing.T) {
	sig := newFakeSignaler()
	pc := &fakePC{answerSDP: "ANSWER"}
	a := NewAnswerer(sig, pc)

	sig.events <- msgOffer(99, "OFFER")
	close(sig.events)
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

func TestAnswererRemoteCandidateAfterOffer(t *testing.T) {
	sig := newFakeSignaler()
	pc := &fakePC{answerSDP: "A"}
	a := NewAnswerer(sig, pc)

	sig.events <- msgOffer(1, "O")
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

// Remote candidates that arrive before the offer are queued and applied after
// SetRemoteDescription.
func TestAnswererQueuesRemoteBeforeOffer(t *testing.T) {
	sig := newFakeSignaler()
	pc := &fakePC{answerSDP: "A"}
	a := NewAnswerer(sig, pc)

	sig.events <- msgCandidate("cand-early")
	if len(pc.added) != 0 { // not applied yet — no remote description
		t.Fatalf("candidate applied before offer: %+v", pc.added)
	}
	sig.events <- msgOffer(5, "O")
	close(sig.events)
	a.Run(context.Background())
	if len(pc.added) != 1 || pc.added[0].Candidate != "cand-early" {
		t.Errorf("queued candidate not applied after offer: %+v", pc.added)
	}
}

// Local candidates gathered before the answer is sent are buffered and flushed
// only after it; later ones send directly; the nil end-of-gathering is skipped.
func TestAnswererLocalICEBufferedThenFlushed(t *testing.T) {
	sig := newFakeSignaler()
	pc := &fakePC{answerSDP: "A"}
	a := NewAnswerer(sig, pc)

	pc.onICE(&webrtc.ICECandidate{Foundation: "x"}) // before answer → buffered
	if len(sig.cands) != 0 {
		t.Fatalf("local candidate sent before answer: %+v", sig.cands)
	}
	sig.events <- msgOffer(77, "O")
	close(sig.events)
	a.Run(context.Background())
	if len(sig.cands) != 1 || sig.cands[0].pid != 77 {
		t.Errorf("buffered local candidate not flushed to peer: %+v", sig.cands)
	}
	pc.onICE(&webrtc.ICECandidate{Foundation: "y"}) // after answer → sent directly
	if len(sig.cands) != 2 {
		t.Errorf("post-answer local candidate not sent: %+v", sig.cands)
	}
	pc.onICE(nil) // end of gathering → not sent
	if len(sig.cands) != 2 {
		t.Errorf("nil candidate should not be sent: %+v", sig.cands)
	}
}

func TestAnswererOfferNoParticipantErrors(t *testing.T) {
	sig := newFakeSignaler()
	a := NewAnswerer(sig, &fakePC{answerSDP: "A"})
	sig.events <- msgOffer(0, "O") // participantId 0
	close(sig.events)
	if err := a.Run(context.Background()); err == nil {
		t.Error("expected error for offer without a participantId")
	}
}

// A single bad remote candidate is logged and skipped, not fatal to Run.
func TestAnswererBadCandidateNotFatal(t *testing.T) {
	sig := newFakeSignaler()
	pc := &fakePC{answerSDP: "A", addErr: context.DeadlineExceeded}
	var logged int
	a := NewAnswerer(sig, pc)
	a.Logf = func(string, ...any) { logged++ }

	sig.events <- msgOffer(1, "O")
	sig.events <- msgCandidate("bad")
	close(sig.events)
	if err := a.Run(context.Background()); err != nil {
		t.Errorf("a bad candidate must not fail Run: %v", err)
	}
	if logged == 0 {
		t.Error("bad candidate should have been logged")
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
