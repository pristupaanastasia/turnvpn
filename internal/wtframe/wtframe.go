// Package wtframe implements the message framing VK Звонки uses on its
// WebTransport signaling channel (reverse-engineered from the web client engine
// chunk 60828.js; see tools/fbdo-capture/signaling-reference.md).
//
// The signaling channel is a single WebTransport bidirectional stream carrying a
// sequence of discrete messages, each framed as:
//
//	[ length prefix ][ payload ]
//	payload = optionally-compressed UTF-8 bytes (JSON on the wire)
//
// The length prefix is a QUIC variable-length integer (RFC 9000 §16): the two
// most-significant bits of the first byte select a 1/2/4/8-byte prefix, and the
// remaining 6/14/30/62 bits hold the payload length. Compression (none, gzip,
// zlib "deflate", or raw DEFLATE) is chosen from the stream URL's query and
// applies to each payload independently.
//
// A sender uses Encoder; a receiver feeds raw stream chunks to Decoder, which
// buffers across chunk boundaries and yields whole messages.
package wtframe

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
)

// Compression identifies how each message payload is (de)compressed. It mirrors
// the modes the web client selects from the WebTransport URL query.
type Compression uint8

const (
	// None leaves payloads uncompressed.
	None Compression = iota
	// GZIP wraps payloads in the gzip format.
	GZIP
	// Deflate wraps payloads in the zlib format (what the client calls "deflate").
	Deflate
	// DeflateRaw uses raw DEFLATE with no zlib/gzip header ("deflate-raw").
	DeflateRaw
)

// ParseCompression maps a stream URL's compression token to a Compression. An
// empty or unrecognized token means None (the client treats unknown as none).
func ParseCompression(token string) Compression {
	switch token {
	case "gzip":
		return GZIP
	case "deflate":
		return Deflate
	case "deflate-raw":
		return DeflateRaw
	default:
		return None
	}
}

// maxVarint is the largest value a QUIC varint can hold (62 bits).
const maxVarint = (uint64(1) << 62) - 1

// DefaultMaxMessage caps a single decoded message when a Decoder sets no
// MaxMessage. Signaling messages are small (a few KB); the generous ceiling just
// bounds memory against a hostile or corrupt stream (a huge length prefix, or a
// decompression bomb).
const DefaultMaxMessage = 16 << 20 // 16 MiB

// AppendUvarint appends v as a QUIC variable-length integer to dst and returns
// the extended slice. It panics if v exceeds the 62-bit varint range, which no
// real message length can reach.
func AppendUvarint(dst []byte, v uint64) []byte {
	switch {
	case v <= 63:
		return append(dst, byte(v))
	case v <= 16383:
		return append(dst, 0x40|byte(v>>8), byte(v))
	case v <= 0x3fffffff:
		return append(dst, 0x80|byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	case v <= maxVarint:
		return append(dst,
			0xc0|byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	default:
		panic(fmt.Sprintf("wtframe: value %d too large for varint", v))
	}
}

// prefixLen returns how many bytes the varint starting at first occupies.
func prefixLen(first byte) int {
	switch first & 0xc0 {
	case 0x00:
		return 1
	case 0x40:
		return 2
	case 0x80:
		return 4
	default:
		return 8
	}
}

// readUvarint decodes a QUIC varint at the start of b. ok is false (with n==0)
// when b does not yet hold the whole prefix, so a caller can wait for more bytes.
func readUvarint(b []byte) (v uint64, n int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	n = prefixLen(b[0])
	if len(b) < n {
		return 0, 0, false
	}
	v = uint64(b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, true
}

// Encoder frames outbound messages with a chosen compression.
type Encoder struct {
	Compression Compression
}

// Encode frames a single message: it compresses the payload (per e.Compression),
// prepends the varint length of the compressed payload, and returns the frame.
func (e Encoder) Encode(msg []byte) ([]byte, error) {
	payload, err := compress(e.Compression, msg)
	if err != nil {
		return nil, err
	}
	out := AppendUvarint(make([]byte, 0, len(payload)+8), uint64(len(payload)))
	return append(out, payload...), nil
}

// Decoder reassembles messages from raw WebTransport stream chunks. It is not safe
// for concurrent use; feed one stream's chunks in order. The zero Decoder uses
// None compression.
type Decoder struct {
	Compression Compression
	// MaxMessage caps one decoded message; <=0 means DefaultMaxMessage. A frame
	// whose declared length — or whose decompressed size — exceeds it is an error.
	MaxMessage int
	buf        []byte
}

func (d *Decoder) maxMessage() int {
	if d.MaxMessage > 0 {
		return d.MaxMessage
	}
	return DefaultMaxMessage
}

// Decode appends chunk to the decoder's buffer and returns every whole message
// now available, in order. A message split across chunks stays buffered until the
// rest arrives. Each returned payload is decompressed and owned by the caller.
//
// A frame whose declared length exceeds MaxMessage is rejected before its bytes
// are buffered, so a hostile length prefix cannot grow memory without bound. On a
// decompress failure the offending frame is consumed before the error returns, so
// a later Decode resumes with the next frame rather than re-failing forever.
func (d *Decoder) Decode(chunk []byte) ([][]byte, error) {
	d.buf = append(d.buf, chunk...)
	max := d.maxMessage()
	var msgs [][]byte
	for {
		length, n, ok := readUvarint(d.buf)
		if !ok {
			break // prefix not fully arrived yet
		}
		if length > uint64(max) {
			return msgs, fmt.Errorf("wtframe: frame length %d exceeds max %d", length, max)
		}
		if uint64(len(d.buf)-n) < length {
			break // payload not fully arrived yet
		}
		payload := d.buf[n : n+int(length)]
		msg, err := decompress(d.Compression, payload, max)
		d.buf = d.buf[n+int(length):] // consume the frame either way
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, msg)
	}
	// Reclaim memory once the backlog is drained so the buffer can't grow forever.
	if len(d.buf) == 0 {
		d.buf = nil
	}
	return msgs, nil
}

// Buffered reports how many bytes are held waiting for the rest of a message.
func (d *Decoder) Buffered() int { return len(d.buf) }

func compress(c Compression, in []byte) ([]byte, error) {
	if c == None {
		return append([]byte(nil), in...), nil
	}
	var buf bytes.Buffer
	var w io.WriteCloser
	var err error
	switch c {
	case GZIP:
		w = gzip.NewWriter(&buf)
	case Deflate:
		w = zlib.NewWriter(&buf)
	case DeflateRaw:
		w, err = flate.NewWriter(&buf, flate.DefaultCompression)
	default:
		return nil, fmt.Errorf("wtframe: unknown compression %d", c)
	}
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(in); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompress(c Compression, in []byte, max int) ([]byte, error) {
	if c == None {
		return append([]byte(nil), in...), nil
	}
	var r io.ReadCloser
	var err error
	switch c {
	case GZIP:
		r, err = gzip.NewReader(bytes.NewReader(in))
	case Deflate:
		r, err = zlib.NewReader(bytes.NewReader(in))
	case DeflateRaw:
		r = flate.NewReader(bytes.NewReader(in))
	default:
		return nil, fmt.Errorf("wtframe: unknown compression %d", c)
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// Bound the inflated size: a small compressed frame must not expand without
	// limit (decompression bomb). Read one byte past max to detect overflow.
	out, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, fmt.Errorf("wtframe: decompressed message exceeds max %d", max)
	}
	return out, nil
}
