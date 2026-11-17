package wtsignal

import (
	"bytes"
	"io"
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

// TestCloseWithStalledConsumer: if nobody drains Messages(), the read loop fills
// its buffer and stalls on delivery; Close must still return promptly (it unblocks
// the delivery via quit) rather than deadlock on <-done.
func TestCloseWithStalledConsumer(t *testing.T) {
	p1, p2 := net.Pipe()
	a := New(p1, Options{})
	b := New(p2, Options{}) // its Messages() is never read

	go func() {
		for i := 0; i < 40; i++ { // more than the 16-slot buffer
			if err := a.Send([]byte("m")); err != nil {
				return // sender unblocks when a is closed below
			}
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the buffer fill and delivery stall

	done := make(chan error, 1)
	go func() { done <- b.Close() }()
	select {
	case <-done:
		// Close returned — no deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung while the consumer was not draining Messages")
	}
	a.Close() // release the blocked sender goroutine
}

// fakeStream yields a fixed chunk once, then io.EOF.
type fakeStream struct {
	data []byte
	done bool
}

func (f *fakeStream) Read(p []byte) (int, error) {
	if f.done {
		return 0, io.EOF
	}
	n := copy(p, f.data)
	f.done = true
	return n, nil
}
func (f *fakeStream) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeStream) Close() error                { return nil }

// TestTruncatedFrameIsError: EOF arriving mid-frame is reported as an error, not a
// clean shutdown.
func TestTruncatedFrameIsError(t *testing.T) {
	// Length prefix declaring 10 bytes, but only 3 payload bytes then EOF.
	partial := wtframe.AppendUvarint(nil, 10)
	partial = append(partial, 1, 2, 3)
	c := New(&fakeStream{data: partial}, Options{})

	// Drain until the channel closes.
	for range c.Messages() {
	}
	if err := c.Err(); err != io.ErrUnexpectedEOF {
		t.Errorf("truncated frame: Err = %v, want ErrUnexpectedEOF", err)
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
