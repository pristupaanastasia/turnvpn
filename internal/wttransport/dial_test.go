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

// TestRedactURL: the token/ua query must not survive into an error string.
func TestRedactURL(t *testing.T) {
	in := "https://videowebrtc.okcdn.ru:23456/wt?token=SECRET&ua=UA%20x&conversationId=c"
	got := redactURL(in)
	want := "https://videowebrtc.okcdn.ru:23456/wt"
	if got != want {
		t.Errorf("redactURL = %q, want %q", got, want)
	}
	if bytes.Contains([]byte(got), []byte("SECRET")) {
		t.Error("redacted URL still contains the token")
	}
	if r := redactURL("::not a url"); r != "<redacted-url>" {
		t.Errorf("unparseable URL = %q, want <redacted-url>", r)
	}
}

// Compile-time assurance that a real *webtransport.Stream satisfies wtStream.
var _ wtStream = (*webtransport.Stream)(nil)
