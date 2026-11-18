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
	version         = 0x05
	CmdConnect      = 0x01 // CONNECT
	CmdUDPAssociate = 0x03 // UDP ASSOCIATE
	atypIPv4        = 0x01
	atypHost        = 0x03
	atypIPv6        = 0x04
)

// Request runs the SOCKS5 no-auth greeting and reads one request, returning its
// command and the requested "host:port". It does NOT send the reply: the caller
// replies once it knows the bind address — ReplyConnect for CONNECT, or
// ReplyUDPAssociate (with the UDP relay socket) for UDP ASSOCIATE.
func Request(conn net.Conn) (cmd byte, target string, err error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return 0, "", fmt.Errorf("socks: greeting: %w", err)
	}
	if head[0] != version {
		return 0, "", fmt.Errorf("socks: not SOCKS5 (ver %d)", head[0])
	}
	if _, err := io.ReadFull(conn, make([]byte, head[1])); err != nil {
		return 0, "", fmt.Errorf("socks: methods: %w", err)
	}
	if _, err := conn.Write([]byte{version, 0x00}); err != nil { // no-auth
		return 0, "", err
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return 0, "", fmt.Errorf("socks: request: %w", err)
	}
	if req[0] != version {
		return 0, "", fmt.Errorf("socks: bad request ver %d", req[0])
	}
	host, err := readAddr(conn, req[3])
	if err != nil {
		return 0, "", err
	}
	portb := make([]byte, 2)
	if _, err := io.ReadFull(conn, portb); err != nil {
		return 0, "", err
	}
	return req[1], net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portb)))), nil
}

// Handshake runs Request, accepts only CONNECT (replying success), and returns the
// target. Other commands are rejected with a SOCKS error reply.
func Handshake(conn net.Conn) (string, error) {
	cmd, target, err := Request(conn)
	if err != nil {
		return "", err
	}
	if cmd != CmdConnect {
		replyError(conn, 0x07) // command not supported
		return "", fmt.Errorf("socks: unsupported command %d", cmd)
	}
	if err := ReplyConnect(conn); err != nil {
		return "", err
	}
	return target, nil
}

// ReplyConnect sends the CONNECT success reply with a dummy bound address (clients
// ignore it for CONNECT).
func ReplyConnect(conn net.Conn) error {
	_, err := conn.Write([]byte{version, 0x00, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// ReplyUDPAssociate sends the UDP ASSOCIATE success reply carrying the relay socket
// address the client must send its datagrams to.
func ReplyUDPAssociate(conn net.Conn, relay *net.UDPAddr) error {
	reply := []byte{version, 0x00, 0x00}
	if v4 := relay.IP.To4(); v4 != nil {
		reply = append(reply, atypIPv4)
		reply = append(reply, v4...)
	} else {
		reply = append(reply, atypIPv6)
		reply = append(reply, relay.IP.To16()...)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(relay.Port))
	reply = append(reply, p[:]...)
	_, err := conn.Write(reply)
	return err
}

// readAddr reads a SOCKS5 address of the given ATYP and returns it as a host string.
func readAddr(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypHost:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("socks: bad atyp %d", atyp)
	}
}

// DecodeUDP parses a SOCKS5 UDP request datagram (RSV RSV FRAG ATYP ADDR PORT DATA),
// returning the target "host:port" and the payload. Fragmentation (FRAG != 0) is
// rejected — we do not reassemble.
func DecodeUDP(p []byte) (target string, payload []byte, err error) {
	if len(p) < 4 {
		return "", nil, fmt.Errorf("socks: short udp datagram")
	}
	if p[2] != 0 {
		return "", nil, fmt.Errorf("socks: udp fragmentation unsupported")
	}
	rest := p[4:]
	var host string
	switch p[3] {
	case atypIPv4:
		if len(rest) < 4 {
			return "", nil, fmt.Errorf("socks: short udp ipv4")
		}
		host, rest = net.IP(rest[:4]).String(), rest[4:]
	case atypIPv6:
		if len(rest) < 16 {
			return "", nil, fmt.Errorf("socks: short udp ipv6")
		}
		host, rest = net.IP(rest[:16]).String(), rest[16:]
	case atypHost:
		if len(rest) < 1 || len(rest) < 1+int(rest[0]) {
			return "", nil, fmt.Errorf("socks: short udp host")
		}
		l := int(rest[0])
		host, rest = string(rest[1:1+l]), rest[1+l:]
	default:
		return "", nil, fmt.Errorf("socks: bad udp atyp %d", p[3])
	}
	if len(rest) < 2 {
		return "", nil, fmt.Errorf("socks: short udp port")
	}
	port := binary.BigEndian.Uint16(rest[:2])
	return net.JoinHostPort(host, strconv.Itoa(int(port))), rest[2:], nil
}

// EncodeUDP wraps payload in a SOCKS5 UDP reply datagram reporting src ("host:port")
// as the source.
func EncodeUDP(src string, payload []byte) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(src)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	out := []byte{0, 0, 0} // RSV RSV FRAG
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			out = append(append(out, atypIPv4), v4...)
		} else {
			out = append(append(out, atypIPv6), ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("socks: host too long")
		}
		out = append(append(out, atypHost, byte(len(host))), host...)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	return append(append(out, p[:]...), payload...), nil
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
