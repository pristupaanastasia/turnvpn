package tunnel

import (
	"sync"

	"github.com/xtaci/smux"

	"github.com/pristupaanastasia/turnvpn/internal/mux"
)

// UDPFlows manages client-side UDP flows over one tunnel's unreliable Lane: each flow
// is set up with a control stream (target + flow id) and then exchanges datagrams on
// the lane, keyed by flow id. It installs the lane's datagram handler, so create one
// per lane (per session). It is what a desktop SOCKS5 UDP ASSOCIATE relay drives.
type UDPFlows struct {
	sess *smux.Session
	lane *mux.Lane

	mu  sync.Mutex
	seq uint32
	m   map[uint32]*ClientUDPFlow
}

// NewUDPFlows installs the datagram handler on lane and returns a flow manager bound
// to sess.
func NewUDPFlows(sess *smux.Session, lane *mux.Lane) *UDPFlows {
	u := &UDPFlows{sess: sess, lane: lane, m: make(map[uint32]*ClientUDPFlow)}
	lane.SetDatagramHandler(u.dispatch)
	return u
}

func (u *UDPFlows) dispatch(flow uint32, payload []byte) {
	u.mu.Lock()
	f := u.m[flow]
	u.mu.Unlock()
	if f == nil {
		return
	}
	select {
	case f.inbound <- payload:
	case <-f.done:
	default: // queue full: drop (UDP has no delivery guarantee)
	}
}

// Open starts a UDP flow to target ("host:port").
func (u *UDPFlows) Open(target string) (*ClientUDPFlow, error) {
	u.mu.Lock()
	u.seq++
	flow := u.seq
	f := &ClientUDPFlow{u: u, flow: flow, inbound: make(chan []byte, 256), done: make(chan struct{})}
	u.m[flow] = f
	u.mu.Unlock()

	ctrl, err := OpenUDP(u.sess, target, flow)
	if err != nil {
		u.remove(flow)
		return nil, err
	}
	f.ctrl = ctrl
	return f, nil
}

func (u *UDPFlows) remove(flow uint32) {
	u.mu.Lock()
	delete(u.m, flow)
	u.mu.Unlock()
}

// ClientUDPFlow is one open UDP flow: datagrams out over the Lane, inbound on a queue.
type ClientUDPFlow struct {
	u       *UDPFlows
	flow    uint32
	ctrl    *smux.Stream
	inbound chan []byte
	done    chan struct{}
	once    sync.Once
}

// Send writes one datagram to the flow's target.
func (f *ClientUDPFlow) Send(b []byte) error {
	_, err := f.u.lane.SendDatagram(f.flow, b)
	return err
}

// Inbound delivers datagrams coming back from the target.
func (f *ClientUDPFlow) Inbound() <-chan []byte { return f.inbound }

// Done is closed when the flow is closed, so inbound pumps can exit.
func (f *ClientUDPFlow) Done() <-chan struct{} { return f.done }

// Close tears the flow down.
func (f *ClientUDPFlow) Close() error {
	f.once.Do(func() { close(f.done) })
	f.u.remove(f.flow)
	if f.ctrl != nil {
		return f.ctrl.Close()
	}
	return nil
}
