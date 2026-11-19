package socks_test

import (
	"bytes"
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/socks"
)

// TestUDPEncodeDecode: a SOCKS5 UDP datagram round-trips its target and payload for
// IPv4, hostname and IPv6 targets.
func TestUDPEncodeDecode(t *testing.T) {
	for _, target := range []string{"1.2.3.4:53", "example.com:443", "[::1]:9999"} {
		payload := []byte("payload-for-" + target)
		enc, err := socks.EncodeUDP(target, payload)
		if err != nil {
			t.Fatalf("EncodeUDP(%s): %v", target, err)
		}
		gotTarget, gotPayload, err := socks.DecodeUDP(enc)
		if err != nil {
			t.Fatalf("DecodeUDP(%s): %v", target, err)
		}
		if gotTarget != target {
			t.Errorf("target = %q, want %q", gotTarget, target)
		}
		if !bytes.Equal(gotPayload, payload) {
			t.Errorf("payload = %q, want %q", gotPayload, payload)
		}
	}
}

// TestDecodeUDPErrors: malformed or fragmented datagrams are rejected.
func TestDecodeUDPErrors(t *testing.T) {
	if _, _, err := socks.DecodeUDP([]byte{0, 0}); err == nil {
		t.Error("short datagram accepted")
	}
	// FRAG != 0 (fragmentation) must be rejected.
	if _, _, err := socks.DecodeUDP([]byte{0, 0, 1, 0x01, 1, 2, 3, 4, 0, 53}); err == nil {
		t.Error("fragmented datagram accepted")
	}
	// Unknown ATYP.
	if _, _, err := socks.DecodeUDP([]byte{0, 0, 0, 0x09, 1, 2}); err == nil {
		t.Error("bad ATYP accepted")
	}
}

func TestUDPEncodeDecodeDomain(t *testing.T) {
	packet, err := socks.EncodeUDP("example.com:53", []byte("query"))
	if err != nil {
		t.Fatalf("EncodeUDP: %v", err)
	}
	target, payload, err := socks.DecodeUDP(packet)
	if err != nil {
		t.Fatalf("DecodeUDP: %v", err)
	}
	if target != "example.com:53" || string(payload) != "query" {
		t.Fatalf("round trip = %q / %q", target, payload)
	}
}
