// Package wggw is the server-side WireGuard exit (gateway) for turnvpn.
//
// It terminates a WireGuard session that arrives over a carrier net.Conn (the
// DTLS-over-TURN tunnel) and forwards the client's traffic to the open internet.
// Unlike a WireGuard endpoint that owns a single address, a gateway must accept
// packets for *any* destination the client routes into the tunnel (allowed_ip
// 0.0.0.0/0). It does this entirely in userspace with a gVisor netstack in
// promiscuous+spoofing mode plus TCP/UDP forwarders: an inbound flow is dialed out
// from the host (net.Dial) and spliced to a gVisor endpoint. No TUN device, no
// kernel WireGuard, no iptables MASQUERADE, no root — which keeps the VPS exit a
// non-root process and lets the whole data plane be tested locally.
//
//	carrier(DTLS) → wgbind → wireguard-go device → linkTun → gvisor stack
//	    → tcp/udp forwarder → Dial(host:port) → open internet
package wggw

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
)

const nicID = 1

// Peer is an authorized WireGuard client: its hex public key and the tunnel
// address range it may use as source, e.g. AllowedIP "10.6.0.2/32".
type Peer struct {
	PubKey    string
	AllowedIP string
}

// Config configures a Gateway.
type Config struct {
	// ServerPrivKey is the gateway's hex-encoded X25519 private key (WireGuard UAPI
	// format). Peers are the clients allowed to connect; a given carrier conn only
	// completes a handshake with the one peer whose key the connecting client holds.
	ServerPrivKey string
	Peers         []Peer

	// MTU of the inner tunnel. Must leave room for WireGuard (~32 B) and the DTLS
	// record inside a TURN ChannelData datagram. Default 1280.
	MTU int

	// Dial opens an outbound connection to a client-requested target. Wire the
	// SSRF guard here to keep the exit from reaching the VPS's own loopback,
	// private ranges or cloud metadata. Default: a plain net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// UDPIdle tears down an idle UDP flow. Default 60s.
	UDPIdle time.Duration

	// Logf logs forwarding errors; default no-op.
	Logf func(string, ...any)

	// Shape paces outbound WireGuard emission to at most this many bytes/sec. 0 = off.
	Shape int
}

// Gateway is a running WireGuard exit over one carrier conn.
type Gateway struct {
	dev     *device.Device
	stk     *stack.Stack
	link    *linkTun
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	udpIdle time.Duration
	logf    func(string, ...any)
}

// New starts a gateway: it builds the netstack, brings a wireguard-go device up
// over the carrier, and begins forwarding. Close stops it. The carrier conn is
// owned by the caller and must be closed separately.
func New(carrier net.Conn, cfg Config) (*Gateway, error) {
	if cfg.MTU == 0 {
		cfg.MTU = 1280
	}
	if cfg.UDPIdle == 0 {
		cfg.UDPIdle = 60 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Dial == nil {
		cfg.Dial = safeDial // SSRF-safe by default
	}

	link := newLinkTun(cfg.MTU)
	stk := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol, ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6,
		},
	})
	if err := stk.CreateNIC(nicID, link.ep); err != nil {
		link.Close()
		return nil, fmt.Errorf("wggw: CreateNIC: %v", err)
	}
	// Accept packets for any destination and let the stack originate replies from
	// those destination addresses — a gateway forwards, it does not own IPs.
	if err := stk.SetPromiscuousMode(nicID, true); err != nil {
		link.Close()
		return nil, fmt.Errorf("wggw: promiscuous: %v", err)
	}
	if err := stk.SetSpoofing(nicID, true); err != nil {
		link.Close()
		return nil, fmt.Errorf("wggw: spoofing: %v", err)
	}
	stk.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nicID})
	stk.AddRoute(tcpip.Route{Destination: header.IPv6EmptySubnet, NIC: nicID})

	g := &Gateway{stk: stk, link: link, dial: cfg.Dial, udpIdle: cfg.UDPIdle, logf: cfg.Logf}

	tcpFwd := tcp.NewForwarder(stk, 0, 2048, g.handleTCP)
	stk.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(stk, g.handleUDP)
	stk.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	if len(cfg.Peers) == 0 {
		link.Close()
		stk.Close()
		return nil, fmt.Errorf("wggw: no peers configured")
	}
	dev := device.NewDevice(link, wgbind.NewWithRate(carrier, cfg.Shape), device.NewLogger(device.LogLevelError, "wggw "))
	var uapi strings.Builder
	fmt.Fprintf(&uapi, "private_key=%s\nlisten_port=0\nreplace_peers=true\n", cfg.ServerPrivKey)
	for _, p := range cfg.Peers {
		fmt.Fprintf(&uapi, "public_key=%s\nallowed_ip=%s\n", p.PubKey, p.AllowedIP)
	}
	if err := dev.IpcSet(uapi.String()); err != nil {
		dev.Close()
		stk.Close()
		return nil, fmt.Errorf("wggw: ipc: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		stk.Close()
		return nil, fmt.Errorf("wggw: up: %w", err)
	}
	g.dev = dev
	return g, nil
}

// Close shuts the gateway down (device, netstack). The carrier conn is not closed.
func (g *Gateway) Close() error {
	if g.dev != nil {
		g.dev.Close()
	}
	g.stk.Close()
	return nil
}

func addrString(a tcpip.Address) string { return net.IP(a.AsSlice()).String() }

// handleTCP dials the requested destination from the host and splices it to the
// gVisor-accepted connection. r.Complete(true) sends an RST when we refuse/fail.
func (g *Gateway) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := net.JoinHostPort(addrString(id.LocalAddress), strconv.Itoa(int(id.LocalPort)))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := g.dial(ctx, "tcp", dst)
	cancel()
	if err != nil {
		g.logf("wggw: tcp dial %s: %v", dst, err)
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		g.logf("wggw: tcp endpoint %s: %v", dst, terr)
		_ = out.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	in := gonet.NewTCPConn(&wq, ep)
	go splice(in, out)
}

// handleUDP dials a UDP socket to the destination and pumps datagrams both ways
// until idle. It returns true: the request is consumed whether or not the dial
// succeeds.
func (g *Gateway) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := net.JoinHostPort(addrString(id.LocalAddress), strconv.Itoa(int(id.LocalPort)))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := g.dial(ctx, "udp", dst)
	cancel()
	if err != nil {
		g.logf("wggw: udp dial %s: %v", dst, err)
		return true
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		g.logf("wggw: udp endpoint %s: %v", dst, terr)
		_ = out.Close()
		return true
	}
	in := gonet.NewUDPConn(&wq, ep)
	go g.pumpUDP(in, out)
	return true
}

// splice copies bidirectionally between two stream conns, half-closing writes as
// each direction ends, then closing both.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}

// pumpUDP relays datagrams between the tunnel-side and host-side UDP conns,
// resetting an idle deadline on each packet; either side erroring tears it down.
func (g *Gateway) pumpUDP(in, out net.Conn) {
	done := make(chan struct{}, 2)
	pump := func(dst, src net.Conn) {
		buf := make([]byte, 64*1024)
		for {
			_ = src.SetReadDeadline(time.Now().Add(g.udpIdle))
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go pump(in, out)
	go pump(out, in)
	<-done
	_ = in.Close()
	_ = out.Close()
	<-done
}

// linkTun bridges a wireguard-go tun.Device to a gVisor channel link endpoint:
// packets WireGuard decrypts from the peer are injected inbound into the stack,
// and packets the stack emits are read back out and encrypted to the peer. This
// mirrors wireguard-go's own netstack bridge, but drives a promiscuous gateway
// stack rather than an address-owning endpoint.
type linkTun struct {
	ep        *channel.Endpoint
	mtu       int
	events    chan tun.Event
	incoming  chan *buffer.View
	closeOnce sync.Once
	closed    chan struct{}
}

func newLinkTun(mtu int) *linkTun {
	lt := &linkTun{
		ep:       channel.New(1024, uint32(mtu), ""),
		mtu:      mtu,
		events:   make(chan tun.Event, 4),
		incoming: make(chan *buffer.View),
		closed:   make(chan struct{}),
	}
	lt.ep.AddNotify(lt)
	lt.events <- tun.EventUp
	return lt
}

// WriteNotify is called by the channel endpoint when the stack has a packet to
// send; move it to the queue wireguard-go reads from.
func (lt *linkTun) WriteNotify() {
	pkt := lt.ep.Read()
	if pkt == nil {
		return
	}
	view := pkt.ToView()
	pkt.DecRef()
	select {
	case lt.incoming <- view:
	case <-lt.closed:
	}
}

func (lt *linkTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case view, ok := <-lt.incoming:
		if !ok {
			return 0, os.ErrClosed
		}
		n, err := view.Read(bufs[0][offset:])
		if err != nil {
			return 0, err
		}
		sizes[0] = n
		return 1, nil
	case <-lt.closed:
		return 0, os.ErrClosed
	}
}

func (lt *linkTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		pkt := b[offset:]
		if len(pkt) == 0 {
			continue
		}
		pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pkt)})
		switch pkt[0] >> 4 {
		case 4:
			lt.ep.InjectInbound(header.IPv4ProtocolNumber, pb)
		case 6:
			lt.ep.InjectInbound(header.IPv6ProtocolNumber, pb)
		}
		// InjectInbound hands the packet to the stack, which releases it; do not
		// DecRef here (matches wireguard-go's own netstack bridge).
	}
	return len(bufs), nil
}

func (lt *linkTun) File() *os.File           { return nil }
func (lt *linkTun) MTU() (int, error)        { return lt.mtu, nil }
func (lt *linkTun) Name() (string, error)    { return "wggw", nil }
func (lt *linkTun) Events() <-chan tun.Event { return lt.events }
func (lt *linkTun) BatchSize() int           { return 1 }

func (lt *linkTun) Close() error {
	lt.closeOnce.Do(func() {
		close(lt.closed)
		lt.ep.Close()
		close(lt.events)
	})
	return nil
}
