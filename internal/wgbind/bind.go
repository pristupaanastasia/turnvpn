// Package wgbind carries WireGuard packets over a single pre-established,
// message-preserving net.Conn instead of a UDP socket.
//
// In turnvpn the carrier is the DTLS-over-TURN datagram channel (TURN ChannelData
// → DTLS record), which the VK relay forwards because it looks like WebRTC media.
// WireGuard normally speaks to a UDP socket through conn.Bind; this Bind replaces
// that socket with the tunnel conn, so a wireguard-go device runs end-to-end over
// the relay. One Bind == one peer (the other end of the tunnel): there is exactly
// one carrier conn, so every packet goes to, and comes from, that single endpoint.
//
// Lifecycle note: wireguard-go opens and closes a Bind repeatedly (on Up, on every
// listen-port/reconfigure, on Down) and expects Close+Open to re-establish the
// transport. A real UDP Bind recreates its socket each time; a carrier conn cannot
// be recreated. So Close here does NOT close the carrier — it only unblocks the
// in-flight receive (via a read deadline) so the device's receive worker exits, and
// Open clears the deadline so a fresh worker reads again on the SAME conn. The
// carrier is owned and closed by the caller (the tunnel), not by the device.
package wgbind

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// Bind is a conn.Bind backed by one carrier net.Conn. The conn MUST preserve
// datagram boundaries (one Write == one WireGuard packet == one Read) and support
// SetReadDeadline — a pion/dtls conn over TURN ChannelData does both.
type Bind struct {
	mu     sync.Mutex
	c      net.Conn
	open   bool
	shaper *shaper // nil = no footprint shaping
}

// New wraps an already-connected, datagram-preserving carrier conn.
func New(c net.Conn) *Bind { return &Bind{c: c} }

// NewWithRate is New plus footprint shaping: outbound WireGuard packets are paced to
// at most bytesPerSec. bytesPerSec <= 0 disables shaping (same as New).
func NewWithRate(c net.Conn, bytesPerSec int) *Bind {
	b := &Bind{c: c}
	if bytesPerSec > 0 {
		b.shaper = newShaper(bytesPerSec)
	}
	return b
}

// carrierEndpoint is the single fixed peer reachable over the carrier. WireGuard
// caches a src/dst per peer; here both are the one tunnel, so the endpoint is a
// constant and roaming is a no-op.
type carrierEndpoint struct{}

func (carrierEndpoint) ClearSrc()           {}
func (carrierEndpoint) SrcToString() string { return "" }
func (carrierEndpoint) DstToString() string { return "turn-dtls-carrier" }
func (carrierEndpoint) DstToBytes() []byte  { return []byte{0} } // mac2 cookie salt; constant is fine for one peer
func (carrierEndpoint) DstIP() netip.Addr   { return netip.Addr{} }
func (carrierEndpoint) SrcIP() netip.Addr   { return netip.Addr{} }

var theEndpoint conn.Endpoint = carrierEndpoint{}

// Open marks the Bind live and returns the single receive function. The "port" is
// meaningless for a carrier conn, so we echo it back as the bound port.
func (b *Bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open = true
	_ = b.c.SetReadDeadline(time.Time{}) // clear any deadline left by a prior Close
	return []conn.ReceiveFunc{b.receive}, port, nil
}

// receive reads exactly one datagram from the carrier into packets[0]. Each read
// is one WireGuard packet, attributed to the single carrier endpoint. While the
// Bind is open the carrier has no read deadline, so any read error means either a
// Close (deadline fired) or a genuinely dead carrier; both report net.ErrClosed so
// the device's receive worker exits cleanly.
func (b *Bind) receive(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
	n, err := b.c.Read(packets[0])
	if err != nil {
		return 0, net.ErrClosed
	}
	sizes[0] = n
	eps[0] = theEndpoint
	return 1, nil
}

// Send writes each packet in bufs to the carrier as its own datagram. ep is
// ignored: there is only one carrier, hence one destination.
func (b *Bind) Send(bufs [][]byte, _ conn.Endpoint) error {
	for _, buf := range bufs {
		if b.shaper != nil {
			b.shaper.take(len(buf))
		}
		if _, err := b.c.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

// Close ends the current open epoch: it unblocks the in-flight receive by arming a
// past read deadline, so the device's receive worker returns net.ErrClosed and
// exits. It deliberately does NOT close the carrier conn — the device may Open
// again, and the carrier's real lifetime is owned by the caller.
func (b *Bind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return nil
	}
	b.open = false
	_ = b.c.SetReadDeadline(time.Now())
	return nil
}

// ParseEndpoint always yields the single carrier endpoint, whatever the UAPI
// "endpoint=" string says (it only needs to be non-empty to arm the peer).
func (b *Bind) ParseEndpoint(string) (conn.Endpoint, error) { return theEndpoint, nil }

// SetMark is a UDP socket option with no meaning for a carrier conn.
func (b *Bind) SetMark(uint32) error { return nil }

// BatchSize is 1: the carrier conn reads and writes one datagram at a time (no
// GSO/recvmmsg batching like a real UDP socket).
func (b *Bind) BatchSize() int { return 1 }
