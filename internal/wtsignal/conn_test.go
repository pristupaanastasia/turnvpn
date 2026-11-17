package wtsignal

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/wtframe"
)

func recvWithin(t *testing.T, c *Conn, d time.Duration) ([]byte, bool) {
	t.Helper()
	select {
	case m, ok := <-c.Messages():
		return m, ok
	case <-time.After(d):
		t.Fatal("timed out waiting for message")
		return nil, false
	}
}

func TestRoundTrip(t *testing.T) {
	for _, comp := range []wtframe.Compression{wtframe.None, wtframe.GZIP, wtframe.Deflate} {
		p1, p2 := net.Pipe()
		a := New(p1, Options{Compression: comp})
		b := New(p2, Options{Compression: comp})

		msgs := [][]byte{
			[]byte(`{"type":"answer"}`),
			[]byte(""),
			bytes.Repeat([]byte("Z"), 100_000), // spans many 32KiB reads
			[]byte("юникод-сообщение"),
		}
		go func() {
			for _, m := range msgs {
				if err := a.Send(m); err != nil {
					t.Errorf("send: %v", err)
					return
				}
			}
		}()
		for i, want := range msgs {
			got, ok := recvWithin(t, b, 5*time.Second)
			if !ok {
				t.Fatalf("comp=%d msg %d: channel closed early", comp, i)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("comp=%d msg %d: len %d != %d", comp, i, len(got), len(want))
			}
		}
		a.Close()
		b.Close()
	}
}

// TestCleanClose: Close stops the read loop (no leaked goroutine — Close waits for
// it), closes Messages, reports no error, and is idempotent.
func TestCleanClose(t *testing.T) {
	p1, p2 := net.Pipe()
	defer p2.Close()
	a := New(p1, Options{})

	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, ok := <-a.Messages(); ok {
		t.Error("Messages not closed after Close")
	}
	if err := a.Err(); err != nil {
		t.Errorf("clean close should have nil Err, got %v", err)
	}
	if err := a.Close(); err != nil { // idempotent
		t.Errorf("second Close: %v", err)
	}
}

// TestPeerCloseStopsLoop: when the peer closes, our read loop exits and Messages
// closes, so no goroutine is left spinning.
func TestPeerCloseStopsLoop(t *testing.T) {
	p1, p2 := net.Pipe()
	a := New(p1, Options{})
	defer a.Close()

	p2.Close()
	select {
	case _, ok := <-a.Messages():
		if ok {
			t.Error("unexpected message after peer close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read loop did not exit after peer close")
	}
}

// TestSendAfterCloseErrors: sending on a closed conn returns an error, not a panic.
func TestSendAfterCloseErrors(t *testing.T) {
	p1, p2 := net.Pipe()
	defer p2.Close()
	a := New(p1, Options{})
	a.Close()
	if err := a.Send([]byte("x")); err == nil {
		t.Error("Send after Close should error")
	}
}
