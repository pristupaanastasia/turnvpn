// Package mux layers a reliable, multiplexed stream session over a single DTLS
// connection. The DTLS connection is itself an unreliable, message-preserving
// datagram channel tunneled through the VK TURN relay, so it cannot carry TCP
// (SOCKS) streams directly. This package puts KCP (a reliable ARQ protocol) under
// smux (stream multiplexing) so many logical streams ride one DTLS/TURN tunnel:
//
//	smux streams  ->  KCP (reliability)  ->  DTLS (encrypt+disguise)  ->  TURN relay
//
// KCP needs a net.PacketConn; a dtls.Conn is a message-preserving net.Conn, so the
// adapter below maps PacketConn ReadFrom/WriteTo straight onto Conn Read/Write.
package mux

import (
	"fmt"
	"net"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

// packetConn adapts a message-preserving net.Conn (a dtls.Conn) to net.PacketConn,
// which is what kcp-go consumes. Each DTLS Read/Write already carries exactly one
// datagram, so ReadFrom/WriteTo map directly onto Read/Write.
type packetConn struct{ net.Conn }

func (p packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := p.Read(b)
	return n, p.RemoteAddr(), err
}

func (p packetConn) WriteTo(b []byte, _ net.Addr) (int, error) { return p.Write(b) }

// tuneKCP sizes the KCP session for a TURN-relayed tunnel: low-latency NoDelay, a
// window that sustains a few Mbit/s, and a conservative MTU that fits a DTLS record
// inside a TURN ChannelData datagram.
func tuneKCP(s *kcp.UDPSession) {
	s.SetNoDelay(1, 20, 2, 1) // nodelay on, 20ms interval, fast resend, no congestion window
	s.SetWindowSize(256, 256)
	s.SetMtu(1200)
	s.SetACKNoDelay(true)
}

// smuxConfig is tuned for a tunnel: generous buffers and a keepalive so a stalled
// link is detected and the session torn down.
func smuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.MaxReceiveBuffer = 4 << 20
	c.MaxStreamBuffer = 1 << 20
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	return c
}

// noneCrypt is required by kcp-go's constructors but is a no-op: DTLS already
// encrypts every datagram, so KCP must not encrypt again.
func noneCrypt() (kcp.BlockCrypt, error) { return kcp.NewNoneBlockCrypt(nil) }

// Client turns a connected DTLS conn into a smux client session (KCP underneath).
func Client(dtlsConn net.Conn) (*smux.Session, error) {
	block, err := noneCrypt()
	if err != nil {
		return nil, err
	}
	kc, err := kcp.NewConn2(dtlsConn.RemoteAddr(), block, 0, 0, packetConn{dtlsConn})
	if err != nil {
		return nil, fmt.Errorf("mux: kcp dial: %w", err)
	}
	tuneKCP(kc)
	sess, err := smux.Client(kc, smuxConfig())
	if err != nil {
		_ = kc.Close()
		return nil, fmt.Errorf("mux: smux client: %w", err)
	}
	return sess, nil
}

// Server turns an accepted DTLS conn into a smux server session (KCP underneath).
func Server(dtlsConn net.Conn) (*smux.Session, error) {
	block, err := noneCrypt()
	if err != nil {
		return nil, err
	}
	ln, err := kcp.ServeConn(block, 0, 0, packetConn{dtlsConn})
	if err != nil {
		return nil, fmt.Errorf("mux: kcp serve: %w", err)
	}
	// One DTLS conn carries exactly one KCP session; accept it (bounded wait).
	if err := ln.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, err
	}
	kc, err := ln.AcceptKCP()
	if err != nil {
		return nil, fmt.Errorf("mux: kcp accept: %w", err)
	}
	_ = kc.SetDeadline(time.Time{})
	tuneKCP(kc)
	sess, err := smux.Server(kc, smuxConfig())
	if err != nil {
		_ = kc.Close()
		return nil, fmt.Errorf("mux: smux server: %w", err)
	}
	return sess, nil
}
