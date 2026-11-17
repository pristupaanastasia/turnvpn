// Package wtsignal turns a single byte stream (the VK Звонки WebTransport
// bidirectional signaling stream) into a message-oriented connection, using the
// wtframe length-prefix framing. It is transport-agnostic: give it any
// io.ReadWriteCloser — a real WebTransport stream in production, or a net.Pipe in
// tests — and it delivers whole signaling messages and sends framed ones back.
package wtsignal

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/pristupaanastasia/turnvpn/internal/wtframe"
)

// readChunk bounds a single Read from the stream.
const readChunk = 32 * 1024

// Conn frames messages over a byte stream. Create it with New; a background read
// loop decodes incoming frames and delivers them on Messages. Send is safe for
// concurrent callers; the stream is written under a mutex.
type Conn struct {
	stream io.ReadWriteCloser
	enc    wtframe.Encoder
	dec    *wtframe.Decoder

	writeMu sync.Mutex

	messages chan []byte
	quit     chan struct{} // closed by Close to unblock a stalled message delivery
	done     chan struct{} // closed when the read loop has exited

	closeOnce sync.Once
	closed    atomic.Bool // set before closing the stream, so readLoop sees a clean stop
	errMu     sync.Mutex
	err       error
}

// Options configures a Conn. The zero value uses no compression and the decoder's
// default message cap.
type Options struct {
	Compression wtframe.Compression
	MaxMessage  int // forwarded to the decoder; <=0 uses wtframe.DefaultMaxMessage
}

// New wraps stream and starts the read loop. Compression must match what the
// peer/stream URL negotiated. The caller owns stream only until Close, which
// closes it.
func New(stream io.ReadWriteCloser, opts Options) *Conn {
	c := &Conn{
		stream:   stream,
		enc:      wtframe.Encoder{Compression: opts.Compression},
		dec:      &wtframe.Decoder{Compression: opts.Compression, MaxMessage: opts.MaxMessage},
		messages: make(chan []byte, 16),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Messages yields decoded signaling messages in order. It is closed when the read
// loop stops (stream EOF/error or Close); after it closes, Err reports why.
func (c *Conn) Messages() <-chan []byte { return c.messages }

// Send frames and writes one message to the stream.
func (c *Conn) Send(msg []byte) error {
	frame, err := c.enc.Encode(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stream.Write(frame); err != nil {
		return fmt.Errorf("wtsignal: write: %w", err)
	}
	return nil
}

// Close closes the underlying stream and waits for the read loop to finish, so no
// goroutine is left running when Close returns. It is idempotent.
func (c *Conn) Close() error {
	var cerr error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.quit)           // unblock a message delivery stalled on a full channel
		cerr = c.stream.Close() // unblock a pending Read
	})
	<-c.done // readLoop has exited
	return cerr
}

// Err returns the error that stopped the read loop, or nil if it stopped on a
// clean EOF or Close. Meaningful only after Messages is closed.
func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func (c *Conn) setErr(err error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
}

func (c *Conn) readLoop() {
	defer close(c.done)
	defer close(c.messages)
	buf := make([]byte, readChunk)
	for {
		n, readErr := c.stream.Read(buf)
		if n > 0 {
			msgs, decErr := c.dec.Decode(buf[:n])
			for _, m := range msgs {
				// Never block forever: Close closes quit so a stalled consumer
				// can't wedge the read loop (and thus Close itself).
				select {
				case c.messages <- m:
				case <-c.quit:
					return
				}
			}
			if decErr != nil {
				c.setErr(decErr)
				return
			}
		}
		if readErr != nil {
			switch {
			case c.closed.Load():
				// Caller-initiated Close: clean stop.
			case readErr == io.EOF:
				// Orderly EOF is clean, unless it arrives mid-frame — then the
				// stream was truncated, which is an error, not a clean shutdown.
				if c.dec.Buffered() > 0 {
					c.setErr(io.ErrUnexpectedEOF)
				}
			default:
				c.setErr(readErr)
			}
			return
		}
	}
}
