package vchat

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type fakeTransport struct {
	in   chan []byte
	mu   sync.Mutex
	sent [][]byte
}

func newFakeTransport() *fakeTransport { return &fakeTransport{in: make(chan []byte, 8)} }

func (f *fakeTransport) Send(b []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, append([]byte(nil), b...))
	f.mu.Unlock()
	return nil
}
func (f *fakeTransport) Messages() <-chan []byte { return f.in }

func (f *fakeTransport) sentContains(pred func([]byte) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sent {
		if pred(s) {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func recv(t *testing.T, c *Conn, d time.Duration) (Message, bool) {
	t.Helper()
	select {
	case m, ok := <-c.Events():
		return m, ok
	case <-time.After(d):
		t.Fatal("timed out waiting for event")
		return Message{}, false
	}
}

func TestConnAnswersPingInternally(t *testing.T) {
	ft := newFakeTransport()
	c := NewConn(ft)
	defer c.Close()

	ft.in <- []byte("ping")
	eventually(t, time.Second, func() bool {
		return ft.sentContains(func(b []byte) bool { return bytes.Equal(b, []byte("pong")) })
	})
	// A ping must not surface as an event; a following real message should.
	ft.in <- []byte(`{"type":"response","response":"x","sequence":1}`)
	m, ok := recv(t, c, time.Second)
	if !ok || m.Kind != KindResponse {
		t.Fatalf("expected the response event, got %+v ok=%v", m, ok)
	}
}

func TestConnForwardsNotifications(t *testing.T) {
	ft := newFakeTransport()
	c := NewConn(ft)
	defer c.Close()

	ft.in <- []byte(`{"type":"notification","notification":"transmitted-data","data":{"sdp":{"type":"offer","sdp":"v=0"}}}`)
	m, ok := recv(t, c, time.Second)
	if !ok {
		t.Fatal("no event")
	}
	sdp, _, got := m.TransmittedData()
	if !got || sdp == nil || sdp.Type != "offer" {
		t.Errorf("offer not forwarded: %+v", m)
	}
}

func TestConnSendSDPAndSequence(t *testing.T) {
	ft := newFakeTransport()
	c := NewConn(ft)
	defer c.Close()

	if err := c.SendSDP(42, SDP{Type: "answer", SDP: "v=0"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SendCandidate(42, Candidate{Candidate: "c", SDPMid: "0"}); err != nil {
		t.Fatal(err)
	}
	var seqs []int
	ft.mu.Lock()
	for _, s := range ft.sent {
		var e struct {
			Command  string `json:"command"`
			Sequence int    `json:"sequence"`
		}
		json.Unmarshal(s, &e)
		if e.Command == "transmit-data" {
			seqs = append(seqs, e.Sequence)
		}
	}
	ft.mu.Unlock()
	if len(seqs) != 2 || seqs[0] == 0 || seqs[1] != seqs[0]+1 {
		t.Errorf("sequences not monotonic: %v", seqs)
	}
}

func TestConnTransportEndClosesEvents(t *testing.T) {
	ft := newFakeTransport()
	c := NewConn(ft)
	close(ft.in) // transport ended
	select {
	case _, ok := <-c.Events():
		if ok {
			t.Error("expected Events closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Events did not close when transport ended")
	}
	c.Close() // idempotent / safe after loop exit
}

func TestConnCloseWithStalledConsumer(t *testing.T) {
	ft := &fakeTransport{in: make(chan []byte, 200)} // big buffer so pushing never blocks the test
	c := NewConn(ft)
	// Queue well past the events buffer and never read, to stall delivery.
	for i := 0; i < 100; i++ {
		ft.in <- []byte(`{"type":"response","response":"x","sequence":1}`)
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung with a stalled consumer")
	}
}
