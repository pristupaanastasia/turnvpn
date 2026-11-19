package dgpipe_test

import (
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
)

func TestPairRoundTrip(t *testing.T) {
	a, b := dgpipe.Pair()
	defer a.Close()
	defer b.Close()

	if _, err := a.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := b.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("read %q", buf[:n])
	}

	if a.LocalAddr().Network() == "" || a.LocalAddr().String() == "" {
		t.Error("addr strings empty")
	}
	if a.RemoteAddr() == nil {
		t.Error("RemoteAddr nil")
	}
	if err := a.SetWriteDeadline(time.Now()); err != nil {
		t.Error(err)
	}
	if err := a.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Error(err)
	}
}

func TestReadAfterClose(t *testing.T) {
	a, b := dgpipe.Pair()
	_ = b.Close()
	_ = a.Close()
	if _, err := a.Read(make([]byte, 4)); err == nil {
		t.Error("read on a closed pipe should error")
	}
}
