package mux

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

// A single DTLS conn carries two multiplexed sub-channels, told apart by a 1-byte
// tag on each datagram:
//
//	tagStream    reliable  — fed to KCP -> smux (TCP/SOCKS streams, UDP control)
//	tagDatagram  unreliable — per-flow datagrams (QUIC and other UDP)
//
// UDP must NOT be forced through the reliable, ordered stream path: a lost packet
// there would head-of-line-block everything behind it, defeating QUIC. The Lane
// gives UDP its own lossy, unordered channel while reusing the same DTLS/TURN
// tunnel (no second allocation).
const (
	tagStream   = 0x00
	tagDatagram = 0x01
	dgHeader    = 5 // tag(1) + flow id(4)
)

// Lane demultiplexes a message-preserving net.Conn (a dtls.Conn) into the reliable
// lane (exposed as a net.PacketConn for KCP) and the unreliable datagram lane.
type Lane struct {
	conn net.Conn

	wmu sync.Mutex // serializes writes to conn (KCP + datagrams share it)

	relCh  chan []byte
	closed chan struct{}
	once   sync.Once

	errMu sync.Mutex
	err   error

	hmu       sync.RWMutex
	dgHandler func(flow uint32, payload []byte)
}

// NewLane wraps conn and starts its read loop. The returned Lane's PacketConn is
// handed to KCP; datagrams are sent/received via SendDatagram / SetDatagramHandler.
func NewLane(conn net.Conn) *Lane {
	l := &Lane{
		conn:   conn,
		relCh:  make(chan []byte, 256),
		closed: make(chan struct{}),
	}
	go l.readLoop()
	return l
}

func (l *Lane) readLoop() {
	buf := make([]byte, 65535)
	for {
		n, err := l.conn.Read(buf)
		if err != nil {
			l.fail(err)
			return
		}
		if n < 1 {
			continue
		}
		switch buf[0] {
		case tagStream:
			p := make([]byte, n-1)
			copy(p, buf[1:n])
			select {
			case l.relCh <- p:
			case <-l.closed:
				return
			}
		case tagDatagram:
			if n < dgHeader {
				continue
			}
			flow := binary.BigEndian.Uint32(buf[1:5])
			p := make([]byte, n-dgHeader)
			copy(p, buf[dgHeader:n])
			l.hmu.RLock()
			h := l.dgHandler
			l.hmu.RUnlock()
			if h != nil {
				h(flow, p)
			}
		}
	}
}

func (l *Lane) fail(err error) {
	l.errMu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.errMu.Unlock()
	l.once.Do(func() { close(l.closed) })
}

func (l *Lane) lastErr() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	if l.err != nil {
		return l.err
	}
	return net.ErrClosed
}

func (l *Lane) writeTagged(tag byte, flow uint32, b []byte) (int, error) {
	out := make([]byte, 0, dgHeader+len(b))
	if tag == tagDatagram {
		var h [dgHeader]byte
		h[0] = tag
		binary.BigEndian.PutUint32(h[1:5], flow)
		out = append(out, h[:]...)
	} else {
		out = append(out, tag)
	}
	out = append(out, b...)
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if _, err := l.conn.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// SendDatagram sends one unreliable datagram tagged with flow.
func (l *Lane) SendDatagram(flow uint32, b []byte) (int, error) {
	return l.writeTagged(tagDatagram, flow, b)
}

// SetDatagramHandler registers the receiver for inbound datagrams. The handler
// runs on the read loop, so it must not block.
func (l *Lane) SetDatagramHandler(h func(flow uint32, payload []byte)) {
	l.hmu.Lock()
	l.dgHandler = h
	l.hmu.Unlock()
}

// PacketConn is the reliable lane, handed to KCP.
func (l *Lane) PacketConn() net.PacketConn { return relConn{l} }

func (l *Lane) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.conn.Close()
}

// relConn presents the reliable lane as a net.PacketConn for kcp-go, which only
// calls ReadFrom/WriteTo (never the deadline methods — verified against v5).
type relConn struct{ l *Lane }

func (r relConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case p := <-r.l.relCh:
		return copy(b, p), r.l.conn.RemoteAddr(), nil
	case <-r.l.closed:
		return 0, nil, r.l.lastErr()
	}
}

func (r relConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return r.l.writeTagged(tagStream, 0, b)
}

func (r relConn) Close() error                       { return r.l.Close() }
func (r relConn) LocalAddr() net.Addr                { return r.l.conn.LocalAddr() }
func (r relConn) SetDeadline(_ time.Time) error      { return nil }
func (r relConn) SetReadDeadline(_ time.Time) error  { return nil }
func (r relConn) SetWriteDeadline(_ time.Time) error { return nil }
