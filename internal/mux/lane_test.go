package mux

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// msgConn is an in-memory, message-preserving net.Conn (each Read returns exactly
// one Write), which is what a dtls.Conn behaves like and what Lane assumes.
type msgConn struct {
	rd   <-chan []byte
	wr   chan<- []byte
	done chan struct{}
	once *sync.Once // shared across the pair so Close is idempotent for both
}

func msgPipe() (*msgConn, *msgConn) {
	ab := make(chan []byte, 64)
	ba := make(chan []byte, 64)
	done := make(chan struct{})
	once := &sync.Once{}
	a := &msgConn{rd: ba, wr: ab, done: done, once: once}
	b := &msgConn{rd: ab, wr: ba, done: done, once: once}
	return a, b
}

func (c *msgConn) Read(b []byte) (int, error) {
	select {
	case m := <-c.rd:
		return copy(b, m), nil
	case <-c.done:
		return 0, io.EOF
	}
}

func (c *msgConn) Write(b []byte) (int, error) {
	m := make([]byte, len(b))
	copy(m, b)
	select {
	case c.wr <- m:
		return len(b), nil
	case <-c.done:
		return 0, io.ErrClosedPipe
	}
}

func (c *msgConn) Close() error                       { c.once.Do(func() { close(c.done) }); return nil }
func (c *msgConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c *msgConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c *msgConn) SetDeadline(_ time.Time) error      { return nil }
func (c *msgConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *msgConn) SetWriteDeadline(_ time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "mem" }
func (dummyAddr) String() string  { return "mem" }

// TestLaneDatagramAndStream: datagrams and reliable frames share one conn without
// corrupting each other, and the flow id round-trips.
func TestLaneDatagramAndStream(t *testing.T) {
	ca, cb := msgPipe()
	la := NewLane(ca)
	lb := NewLane(cb)
	defer la.Close()
	defer lb.Close()

	got := make(chan struct {
		flow uint32
		data []byte
	}, 8)
	lb.SetDatagramHandler(func(flow uint32, payload []byte) {
		got <- struct {
			flow uint32
			data []byte
		}{flow, append([]byte(nil), payload...)}
	})

	// Interleave a reliable frame and two datagrams from A.
	if _, err := la.PacketConn().WriteTo([]byte("reliable-1"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := la.SendDatagram(7, []byte("dgram-A")); err != nil {
		t.Fatal(err)
	}
	if _, err := la.SendDatagram(2600, []byte("dgram-B")); err != nil {
		t.Fatal(err)
	}

	// Reliable frame arrives intact on B's PacketConn (tag stripped).
	buf := make([]byte, 64)
	done := make(chan struct{})
	var rn int
	go func() { rn, _, _ = lb.PacketConn().ReadFrom(buf); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reliable read timed out")
	}
	if string(buf[:rn]) != "reliable-1" {
		t.Fatalf("reliable = %q, want reliable-1", buf[:rn])
	}

	// Both datagrams arrive with correct flow ids and payloads.
	want := map[uint32]string{7: "dgram-A", 2600: "dgram-B"}
	for i := 0; i < 2; i++ {
		select {
		case g := <-got:
			if want[g.flow] != string(g.data) {
				t.Fatalf("flow %d = %q, want %q", g.flow, g.data, want[g.flow])
			}
			delete(want, g.flow)
		case <-time.After(2 * time.Second):
			t.Fatal("datagram timed out")
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing datagrams: %v", want)
	}
}
