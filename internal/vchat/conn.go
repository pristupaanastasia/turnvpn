package vchat

import (
	"sync"
	"sync/atomic"
)

// Transport is the message-oriented signaling transport vchat runs over: it sends
// whole messages and delivers inbound ones on a channel that closes when the
// transport ends. *wtsignal.Conn satisfies it.
type Transport interface {
	Send([]byte) error
	Messages() <-chan []byte
}

// Conn drives the vchat protocol over a Transport: it parses inbound messages,
// answers heartbeats itself, and surfaces the rest as typed events. Outbound
// commands get an auto-incrementing sequence. Create it with NewConn.
type Conn struct {
	t      Transport
	events chan Message
	seq    atomic.Int64

	closeOnce sync.Once
	quit      chan struct{}
	done      chan struct{}
}

// NewConn starts driving t and returns the Conn. Close it when done.
func NewConn(t Transport) *Conn {
	c := &Conn{
		t:      t,
		events: make(chan Message, 32),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go c.loop()
	return c
}

// Events delivers inbound notifications and responses in order. Heartbeat pings
// are answered internally and never appear here. The channel closes when the
// transport ends or Close is called.
func (c *Conn) Events() <-chan Message { return c.events }

// NextSequence returns the next outbound command sequence number.
func (c *Conn) NextSequence() int { return int(c.seq.Add(1)) }

// SendSDP sends an offer or answer to a participant as a transmit-data command.
func (c *Conn) SendSDP(participantID int64, sdp SDP) error {
	b, err := TransmitSDP(c.NextSequence(), participantID, sdp)
	if err != nil {
		return err
	}
	return c.t.Send(b)
}

// SendCandidate trickles an ICE candidate to a participant.
func (c *Conn) SendCandidate(participantID int64, cand Candidate) error {
	b, err := TransmitCandidate(c.NextSequence(), participantID, cand)
	if err != nil {
		return err
	}
	return c.t.Send(b)
}

// Close stops the driver and waits for its loop to exit.
func (c *Conn) Close() {
	c.closeOnce.Do(func() { close(c.quit) })
	<-c.done
}

func (c *Conn) loop() {
	defer close(c.done)
	defer close(c.events)
	in := c.t.Messages()
	for {
		select {
		case <-c.quit:
			return
		case raw, ok := <-in:
			if !ok {
				return // transport ended
			}
			msg, err := Parse(raw)
			if err != nil {
				continue // ignore an unparseable frame rather than tear down
			}
			if msg.IsPing() {
				_ = c.t.Send(Pong()) // best-effort heartbeat reply
				continue
			}
			select {
			case c.events <- msg:
			case <-c.quit:
				return
			}
		}
	}
}
