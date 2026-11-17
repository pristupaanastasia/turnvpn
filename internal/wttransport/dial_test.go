package wttransport

import (
	"bytes"
	"testing"

	"github.com/quic-go/webtransport-go"
)

// fakeWTStream records the lifecycle calls streamRWC makes.
type fakeWTStream struct {
	readData          []byte
	readPos           int
	written           []byte
	cancelled         bool
	closed            bool
	cancelBeforeClose bool
}

func (f *fakeWTStream) Read(p []byte) (int, error) {
	if f.readPos >= len(f.readData) {
		return 0, bytes.ErrTooLarge // arbitrary non-nil to end reads in tests
	}
	n := copy(p, f.readData[f.readPos:])
	f.readPos += n
	return n, nil
}
func (f *fakeWTStream) Write(p []byte) (int, error) {
	f.written = append(f.written, p...)
	return len(p), nil
}
func (f *fakeWTStream) Close() error {
	f.closed = true
	f.cancelBeforeClose = f.cancelled
	return nil
}
func (f *fakeWTStream) CancelRead(webtransport.StreamErrorCode) { f.cancelled = true }

// TestStreamRWCClosesCancelRead: Close must cancel the read side before closing,
// so a blocked Read is unblocked (the QUIC-stream Close caveat from review).
func TestStreamRWCClosesCancelRead(t *testing.T) {
	f := &fakeWTStream{readData: []byte("hello")}
	w := streamRWC{s: f}

	buf := make([]byte, 5)
	n, _ := w.Read(buf)
	if string(buf[:n]) != "hello" {
		t.Errorf("Read passthrough = %q", buf[:n])
	}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if string(f.written) != "abc" {
		t.Errorf("Write passthrough = %q", f.written)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.cancelled {
		t.Error("Close did not CancelRead")
	}
	if !f.closed {
		t.Error("Close did not close the stream")
	}
	if !f.cancelBeforeClose {
		t.Error("CancelRead must happen before Close so a pending Read unblocks")
	}
}

// Compile-time assurance that a real *webtransport.Stream satisfies wtStream.
var _ wtStream = (*webtransport.Stream)(nil)
