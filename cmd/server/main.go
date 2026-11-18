// Command server is the VPS-side exit of the Vector 1 tunnel.
//
// For Stage 0 (the make-or-break relay test) it is a DTLS echo server: it listens
// for DTLS over UDP — the packets the VK TURN relay forwards to it — and echoes back
// whatever it receives. That lets the client confirm that DTLS-wrapped data actually
// crosses the VK relay to an arbitrary external peer (this box), which an earlier
// raw-UDP probe wrongly reported as impossible.
//
// Later stages grow this into the real exit (DTLS -> mux -> dial the open internet).
package main

import (
	"flag"
	"log"
	"net"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:56000", "UDP address to listen on for DTLS")
	flag.Parse()

	laddr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatalf("server: resolve %q: %v", *listen, err)
	}
	// Self-signed is fine: DTLS here is a transport wrapper (disguise + channel
	// encryption), not an identity system. The client uses InsecureSkipVerify.
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		log.Fatalf("server: self-signed cert: %v", err)
	}

	ln, err := dtls.ListenWithOptions("udp", laddr,
		dtls.WithCertificates(cert),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
	)
	if err != nil {
		log.Fatalf("server: dtls listen on %s: %v", *listen, err)
	}
	defer func() { _ = ln.Close() }()
	log.Printf("server: DTLS echo listening on %s", *listen)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("server: accept: %v", err)
			continue
		}
		go echo(conn)
	}
}

// echo reads datagrams from one DTLS connection and writes them straight back.
func echo(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	ra := conn.RemoteAddr()
	log.Printf("server: DTLS connection from %s", ra)
	buf := make([]byte, 64*1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Printf("server: %s read: %v", ra, err)
			return
		}
		log.Printf("server: echo %d bytes from %s", n, ra)
		if _, err := conn.Write(buf[:n]); err != nil {
			log.Printf("server: %s write: %v", ra, err)
			return
		}
	}
}
