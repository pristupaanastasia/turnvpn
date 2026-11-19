// Package kernelexit is the server-side "kernel exit" (variant B): instead of
// terminating the client's WireGuard session in userspace (gVisor netstack, see
// package wggw), it relays the raw WireGuard datagrams between the DTLS carrier
// and a kernel WireGuard interface listening on localhost.
//
// Layering: the carrier (TURN + DTLS) and the client's WireGuard identity are
// unchanged — the client cannot tell which exit terminates it. All this package
// does is shuttle whole WireGuard messages, 1:1, between the carrier conn and a
// connected UDP socket to 127.0.0.1:<wg-port>. The kernel does the crypto,
// routing and NAT; it identifies the peer cryptographically (by its static key),
// so every client may relay to the same wg port from its own UDP socket and the
// kernel sends each peer's return traffic back to that socket.
//
// IMPORTANT — SSRF: the kernel with MASQUERADE will forward anywhere, including
// 127.0.0.1, link-local 169.254.169.254 (cloud metadata) and RFC1918 ranges.
// Unlike the userspace exit, this package does NOT enforce an egress policy; the
// kernel exit is only safe behind the nftables drop rules installed by
// scripts/kernel-exit-setup.sh. Do not run -exit kernel without them.
//
// The Go side is deliberately tiny and OS-portable (just a UDP relay), so it
// builds and is tested on any platform; only the wg0 interface and nft ruleset it
// talks to are Linux- and root-specific, and those live in the setup script.
package kernelexit

import (
	"fmt"
	"net"
	"sync"
)

// maxDatagram bounds a single relayed WireGuard message. WireGuard data messages
// are at most the tunnel MTU plus a small fixed header; handshake messages are
// smaller. 2048 leaves ample room for any inner MTU we use (<=1420).
const maxDatagram = 2048

// Relay shuttles WireGuard datagrams between one carrier conn and a connected UDP
// socket to the kernel WireGuard listen port. One Relay == one client.
type Relay struct {
	carrier net.Conn
	udp     *net.UDPConn

	closeOnce sync.Once
	done      chan struct{}
}

// New dials a connected UDP socket to 127.0.0.1:wgPort. Each New gets its own
// local port, which the kernel uses as that peer's endpoint for return traffic.
// The carrier is owned by the caller and is NOT closed by the Relay.
func New(carrier net.Conn, wgPort int) (*Relay, error) {
	if wgPort <= 0 || wgPort > 65535 {
		return nil, fmt.Errorf("kernelexit: bad wg port %d", wgPort)
	}
	raddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: wgPort}
	udp, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("kernelexit: dial kernel wg 127.0.0.1:%d: %w", wgPort, err)
	}
	return &Relay{carrier: carrier, udp: udp, done: make(chan struct{})}, nil
}

// Start launches the two pump goroutines (carrier->kernel and kernel->carrier) and
// returns immediately. When either direction errors (carrier closed by the caller,
// or Close) both pumps unwind and done is closed.
func (r *Relay) Start() {
	var once sync.Once
	stop := func() { once.Do(func() { close(r.done); _ = r.udp.Close() }) }

	go r.pump(r.carrier, r.udp, stop) // client -> kernel wg
	go r.pump(r.udp, r.carrier, stop) // kernel wg -> client
}

// pump copies whole datagrams from src to dst until either errors, then calls stop
// so the sibling pump also unwinds. src and dst are message-preserving (the
// carrier preserves DTLS/TURN datagram boundaries; a UDP socket is inherently
// per-datagram), so one Read is exactly one WireGuard message and one Write.
func (r *Relay) pump(src, dst net.Conn, stop func()) {
	defer stop()
	buf := make([]byte, maxDatagram)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Done is closed when both pumps have stopped (carrier dead or Close called).
func (r *Relay) Done() <-chan struct{} { return r.done }

// Close stops the relay by closing the UDP socket, which unblocks the
// kernel->carrier pump; the carrier->kernel pump unwinds when the caller closes
// the carrier. The carrier itself is not closed here (the caller owns it).
func (r *Relay) Close() error {
	r.closeOnce.Do(func() { _ = r.udp.Close() })
	return nil
}
