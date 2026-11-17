package wtframe

import (
	"bytes"
	"fmt"
	"testing"
)

// TestVarintVectors pins the QUIC varint byte layout to exact vectors, matching
// the web client's encoder (chunk 60828.js, class a_). These must not drift.
func TestVarintVectors(t *testing.T) {
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{63, []byte{0x3f}},                                 // max 1-byte
		{64, []byte{0x40, 0x40}},                           // min 2-byte
		{16383, []byte{0x7f, 0xff}},                        // max 2-byte
		{16384, []byte{0x80, 0x00, 0x40, 0x00}},            // min 4-byte
		{0x3fffffff, []byte{0xbf, 0xff, 0xff, 0xff}},       // max 4-byte
		{0x40000000, []byte{0xc0, 0, 0, 0, 0x40, 0, 0, 0}}, // min 8-byte
	}
	for _, c := range cases {
		got := AppendUvarint(nil, c.v)
		if !bytes.Equal(got, c.want) {
			t.Errorf("AppendUvarint(%d) = % x, want % x", c.v, got, c.want)
		}
		v, n, ok := readUvarint(got)
		if !ok || n != len(c.want) || v != c.v {
			t.Errorf("readUvarint(% x) = (%d,%d,%v), want (%d,%d,true)", got, v, n, ok, c.v, len(c.want))
		}
	}
}

// TestVarintRoundTrip checks encode/decode across range boundaries.
func TestVarintRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 62, 63, 64, 100, 16382, 16383, 16384, 1 << 20, 0x3ffffffe, 0x3fffffff, 0x40000000, 1 << 40, maxVarint} {
		b := AppendUvarint(nil, v)
		got, n, ok := readUvarint(b)
		if !ok || got != v || n != len(b) {
			t.Errorf("round-trip %d: got (%d,%d,%v)", v, got, n, ok)
		}
	}
}

// TestReadUvarintIncomplete: a truncated prefix must report not-ok, not panic.
func TestReadUvarintIncomplete(t *testing.T) {
	full := AppendUvarint(nil, 70000) // 4-byte prefix
	for i := 0; i < len(full); i++ {
		if _, _, ok := readUvarint(full[:i]); ok {
			t.Errorf("readUvarint of %d/%d bytes reported ok", i, len(full))
		}
	}
	if _, _, ok := readUvarint(full); !ok {
		t.Error("readUvarint of full prefix reported not ok")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, comp := range []Compression{None, GZIP, Deflate, DeflateRaw} {
		t.Run(fmt.Sprintf("comp=%d", comp), func(t *testing.T) {
			enc := Encoder{Compression: comp}
			msgs := [][]byte{
				[]byte(`{"type":"offer","sdp":"v=0..."}`),
				[]byte(""),
				bytes.Repeat([]byte("A"), 20000), // >16383 so a 4-byte prefix is exercised
				[]byte("короткое юникод-сообщение"),
			}
			var stream []byte
			for _, m := range msgs {
				f, err := enc.Encode(m)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				stream = append(stream, f...)
			}
			dec := &Decoder{Compression: comp}
			got, err := dec.Decode(stream)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != len(msgs) {
				t.Fatalf("got %d msgs, want %d", len(got), len(msgs))
			}
			for i := range msgs {
				if !bytes.Equal(got[i], msgs[i]) {
					t.Errorf("msg %d mismatch (len %d vs %d)", i, len(got[i]), len(msgs[i]))
				}
			}
			if dec.Buffered() != 0 {
				t.Errorf("decoder left %d bytes buffered", dec.Buffered())
			}
		})
	}
}

// TestDecoderSplitAcrossChunks feeds the stream one byte at a time: the decoder
// must reassemble messages that span arbitrary chunk boundaries (the real
// WebTransport read loop delivers arbitrary slices).
func TestDecoderSplitAcrossChunks(t *testing.T) {
	enc := Encoder{Compression: None}
	msgs := [][]byte{
		[]byte("first"),
		bytes.Repeat([]byte("x"), 300),
		[]byte("third-message-here"),
	}
	var stream []byte
	for _, m := range msgs {
		f, _ := enc.Encode(m)
		stream = append(stream, f...)
	}
	dec := &Decoder{Compression: None}
	var got [][]byte
	for i := 0; i < len(stream); i++ {
		out, err := dec.Decode(stream[i : i+1])
		if err != nil {
			t.Fatalf("decode byte %d: %v", i, err)
		}
		got = append(got, out...)
	}
	if len(got) != len(msgs) {
		t.Fatalf("got %d msgs, want %d", len(got), len(msgs))
	}
	for i := range msgs {
		if !bytes.Equal(got[i], msgs[i]) {
			t.Errorf("msg %d mismatch", i)
		}
	}
}

// TestDecoderMultipleInOneChunk: several whole messages in a single Decode call.
func TestDecoderMultipleInOneChunk(t *testing.T) {
	enc := Encoder{Compression: None}
	var stream []byte
	n := 5
	for i := 0; i < n; i++ {
		f, _ := enc.Encode([]byte(fmt.Sprintf("msg-%d", i)))
		stream = append(stream, f...)
	}
	dec := &Decoder{Compression: None}
	got, err := dec.Decode(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("got %d, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		if string(got[i]) != fmt.Sprintf("msg-%d", i) {
			t.Errorf("msg %d = %q", i, got[i])
		}
	}
}

func TestParseCompression(t *testing.T) {
	cases := map[string]Compression{
		"gzip": GZIP, "deflate": Deflate, "deflate-raw": DeflateRaw,
		"": None, "unknown": None, "br": None,
	}
	for in, want := range cases {
		if got := ParseCompression(in); got != want {
			t.Errorf("ParseCompression(%q) = %d, want %d", in, got, want)
		}
	}
}
