package tunnel_test

import (
	"net"
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/tunnel"
)

func TestClientUDPFlows(t *testing.T) {
	orig := tunnel.TargetAllowed
	tunnel.TargetAllowed = func(net.IP) bool { return true }
	// Restore via Cleanup (LIFO) so it runs after buildTunnel's cleanup has stopped
	// the Serve goroutine that reads TargetAllowed — otherwise the restore races it.
	t.Cleanup(func() { tunnel.TargetAllowed = orig })

	// UDP echo origin.
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDP(buf[:n], addr)
		}
	}()

	sess, lane := buildTunnel(t, testSecret, testSecret)
	flows := tunnel.NewUDPFlows(sess, lane)

	f, err := flows.Open(pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	deadline := time.After(10 * time.Second)
	got := false
	for !got {
		if err := f.Send([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		select {
		case p := <-f.Inbound():
			if string(p) != "ping" {
				t.Fatalf("echo = %q", p)
			}
			got = true
		case <-time.After(250 * time.Millisecond):
		case <-deadline:
			t.Fatal("no UDP echo")
		}
	}

	if err := f.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case <-f.Done():
	case <-time.After(2 * time.Second):
		t.Error("Done() did not close after Close()")
	}
}
