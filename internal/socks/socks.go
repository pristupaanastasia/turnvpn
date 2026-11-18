// Package socks implements the minimal client-facing SOCKS5 the tunnel exposes
// locally (CONNECT only — enough for a browser's TCP traffic), plus the tiny
// per-stream target header the client prepends to each tunnel stream so the server
// knows where to dial. The SOCKS5 wire format is RFC 1928.
package socks

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
)

// SOCKS5 constants (RFC 1928).
const (
	version  = 0x05
	cmdConn  = 0x01 // CONNECT
	atypIPv4 = 0x01
	atypHost = 0x03
	atypIPv6 = 0x04
)

// Handshake runs the SOCKS5 no-auth greeting, reads a CONNECT request from conn,
// replies success, and returns the requested target as "host:port". Only CONNECT is
// supported; other commands or address types are rejected with a SOCKS error reply.
func Handshake(conn net.Conn) (string, error) {
	// Greeting: VER, NMETHODS, METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return "", fmt.Errorf("socks: greeting: %w", err)
	}
	if head[0] != version {
		return "", fmt.Errorf("socks: not SOCKS5 (ver %d)", head[0])
	}
	if _, err := io.ReadFull(conn, make([]byte, head[1])); err != nil {
		return "", fmt.Errorf("socks: methods: %w", err)
	}
	// Reply: VER, METHOD=0 (no auth).
	if _, err := conn.Write([]byte{version, 0x00}); err != nil {
		return "", err
	}

	// Request: VER, CMD, RSV, ATYP, ADDR, PORT.
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return "", fmt.Errorf("socks: request: %w", err)
	}
	if req[0] != version {
		return "", fmt.Errorf("socks: bad request ver %d", req[0])
	}
	if req[1] != cmdConn {
		replyError(conn, 0x07) // command not supported
		return "", fmt.Errorf("socks: unsupported command %d", req[1])
	}

	var host string
	switch req[3] {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case atypHost:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		host = string(b)
	default:
		replyError(conn, 0x08) // address type not supported
		return "", fmt.Errorf("socks: bad atyp %d", req[3])
	}

	portb := make([]byte, 2)
	if _, err := io.ReadFull(conn, portb); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portb)

	// Success reply (VER, REP=0, RSV, ATYP=IPv4, BND.ADDR=0, BND.PORT=0). We send a
	// dummy bound address; browsers ignore it for CONNECT.
	if _, err := conn.Write([]byte{version, 0x00, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// replyError sends a SOCKS5 failure reply with the given REP code (best effort).
func replyError(conn net.Conn, code byte) {
	_, _ = conn.Write([]byte{version, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
}

// maxTarget bounds a target string read from the tunnel (defensive).
const maxTarget = 1 << 10

// WriteTarget prepends a tunnel stream with its destination: a uvarint length then
// the "host:port" bytes. The server reads this with ReadTarget before piping.
func WriteTarget(w io.Writer, target string) error {
	if len(target) == 0 || len(target) > maxTarget {
		return fmt.Errorf("socks: target length %d out of range", len(target))
	}
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(target)))
	if _, err := w.Write(hdr[:n]); err != nil {
		return err
	}
	_, err := io.WriteString(w, target)
	return err
}

// ReadTarget reads a header written by WriteTarget and returns the "host:port".
func ReadTarget(r io.Reader) (string, error) {
	l, err := binary.ReadUvarint(byteReader{r})
	if err != nil {
		return "", fmt.Errorf("socks: read target length: %w", err)
	}
	if l == 0 || l > maxTarget {
		return "", fmt.Errorf("socks: target length %d out of range", l)
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("socks: read target: %w", err)
	}
	return string(b), nil
}

// byteReader adapts an io.Reader to io.ByteReader for binary.ReadUvarint.
type byteReader struct{ r io.Reader }

func (b byteReader) ReadByte() (byte, error) {
	var p [1]byte
	_, err := io.ReadFull(b.r, p[:])
	return p[0], err
}
