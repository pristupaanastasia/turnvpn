package kernelexit

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
)

// fakeKernelWG stands in for the kernel WireGuard listen socket: a UDP server that
// echoes every datagram back to its sender, prefixed so the test can tell the
// return path apart from the forward path. It returns the port it listened on.
func fakeKernelWG(t *testing.T) (port int, stop func()) {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen fake kernel wg: %v", err)
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDP(append([]byte("R:"), buf[:n]...), from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port, func() { _ = pc.Close() }
}

func TestRelayRoundTrip(t *testing.T) {
	port, stopKernel := fakeKernelWG(t)
	defer stopKernel()

	clientEnd, serverEnd := dgpipe.Pair() // serverEnd is the carrier the relay drives
	r, err := New(serverEnd, port)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Start()
	defer r.Close()

	// A WireGuard message from the client goes carrier -> relay -> kernel; the
	// kernel's reply comes back kernel -> relay -> carrier.
	msg := []byte("wg-handshake-datagram")
	if _, err := clientEnd.Write(msg); err != nil {
		t.Fatalf("client write: %v", err)
	}

	_ = clientEnd.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 2048)
	n, err := clientEnd.Read(got)
	if err != nil {
		t.Fatalf("client read reply: %v", err)
	}
	want := append([]byte("R:"), msg...)
	if !bytes.Equal(got[:n], want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got[:n], want)
	}
}

// TestRelayDatagramBoundaries verifies each datagram is relayed 1:1 (one WireGuard
// message per Read/Write), never coalesced — essential, since WireGuard needs
// message framing.
func TestRelayDatagramBoundaries(t *testing.T) {
	port, stopKernel := fakeKernelWG(t)
	defer stopKernel()

	clientEnd, serverEnd := dgpipe.Pair()
	r, err := New(serverEnd, port)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Start()
	defer r.Close()

	msgs := [][]byte{[]byte("one"), []byte("two-longer"), []byte("3")}
	for _, m := range msgs {
		if _, err := clientEnd.Write(m); err != nil {
			t.Fatalf("write %q: %v", m, err)
		}
	}
	_ = clientEnd.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 2048)
	for _, m := range msgs {
		n, err := clientEnd.Read(got)
		if err != nil {
			t.Fatalf("read for %q: %v", m, err)
		}
		want := append([]byte("R:"), m...)
		if !bytes.Equal(got[:n], want) {
			t.Fatalf("boundary: got %q want %q", got[:n], want)
		}
	}
}

func TestRelayCloseUnblocks(t *testing.T) {
	port, stopKernel := fakeKernelWG(t)
	defer stopKernel()

	_, serverEnd := dgpipe.Pair()
	r, err := New(serverEnd, port)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Start()
	r.Close()

	select {
	case <-r.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop the relay")
	}
}

func TestNewBadPort(t *testing.T) {
	_, serverEnd := dgpipe.Pair()
	for _, p := range []int{0, -1, 70000} {
		if _, err := New(serverEnd, p); err == nil {
			t.Errorf("New(port=%d): want error", p)
		}
	}
}
