package vpntun

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/mux"
	"github.com/pristupaanastasia/turnvpn/internal/tunnel"
)

func TestNewValidation(t *testing.T) {
	creds := func(context.Context) (string, string, string, error) { return "", "", "", nil }
	if _, err := New(Config{}); err == nil {
		t.Error("empty config should error")
	}
	if _, err := New(Config{PeerAddr: "1.2.3.4:5", Secret: "s", Creds: creds}); err != nil {
		t.Errorf("valid config errored: %v", err)
	}
	if _, err := New(Config{PeerAddr: "bad addr", Secret: "s", Creds: creds}); err == nil {
		t.Error("bad peer address should error")
	}
}

func TestTurnHostPort(t *testing.T) {
	if got := turnHostPort("turn:host:3478?transport=udp"); got != "host:3478" {
		t.Errorf("turnHostPort = %q", got)
	}
}

func TestTokenCreds(t *testing.T) {
	cf := TokenCreds("appkey", "link", "token")
	if cf == nil {
		t.Fatal("TokenCreds returned nil")
	}
}

// injectDialer wires a Dialer to a live mux session connected to a server running
// tunnel.Serve, over an in-memory carrier — skipping establish()/TURN so the Dial
// and UDP paths can be exercised directly.
func injectDialer(t *testing.T) *Dialer {
	t.Helper()
	// Relax the exit guard so loopback origins are reachable.
	orig := tunnel.TargetAllowed
	tunnel.TargetAllowed = func(net.IP) bool { return true }
	t.Cleanup(func() { tunnel.TargetAllowed = orig })

	a, b := dgpipe.Pair()
	go func() {
		sess, lane, err := mux.Server(b)
		if err != nil {
			return
		}
		tunnel.Serve(sess, lane, func(string, ...any) {})
	}()
	sess, lane, err := mux.Client(a)
	if err != nil {
		t.Fatalf("mux client: %v", err)
	}
	d := &Dialer{cfg: Config{Logf: func(string, ...any) {}}, flows: make(map[uint32]chan []byte)}
	d.sess = sess
	d.lane = lane
	lane.SetDatagramHandler(d.dispatch)
	t.Cleanup(func() { _ = d.Close(); _ = a.Close(); _ = b.Close() })
	return d
}

func TestDialerTCPAndUDP(t *testing.T) {
	// TCP echo origin.
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpLn.Close()
	go func() {
		for {
			c, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(c, c); _ = c.Close() }(c)
		}
	}()

	// UDP echo origin.
	udpPc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udpPc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := udpPc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = udpPc.WriteToUDP(buf[:n], addr)
		}
	}()

	d := injectDialer(t)

	// TCP through the tunnel.
	conn, err := d.Dial(context.Background(), "tcp", tcpLn.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("tcp echo = %q", buf)
	}
	_ = conn.Close()

	// UDP through the tunnel.
	f, err := d.OpenUDP(udpPc.LocalAddr().String())
	if err != nil {
		t.Fatalf("OpenUDP: %v", err)
	}
	// Retry: the first datagram can arrive before the server has bound the socket
	// from the control stream, and UDP has no delivery guarantee.
	deadline := time.After(10 * time.Second)
	for {
		if _, err := f.Send([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-f.Inbound():
			if string(got) != "ping" {
				t.Fatalf("udp echo = %q", got)
			}
			_ = f.Close()
			return
		case <-time.After(250 * time.Millisecond):
			// resend
		case <-deadline:
			t.Fatal("no UDP echo")
		}
	}
}

func TestMarkDeadAndClose(t *testing.T) {
	d := injectDialer(t)
	// Kill the current session; markDead clears it and tears the carrier down.
	d.markDead(d.sess)
	// With no real creds, re-establishing fails fast (before any TURN work).
	d.cfg.Creds = func(context.Context) (string, string, string, error) {
		return "", "", "", io.ErrUnexpectedEOF
	}
	if _, err := d.session(); err == nil {
		t.Error("expected establish to fail without creds")
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close is idempotent.
	if err := d.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Dialing a closed dialer errors.
	if _, err := d.Dial(context.Background(), "tcp", "1.2.3.4:5"); err == nil {
		t.Error("Dial on a closed dialer should error")
	}
}
