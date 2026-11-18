// Command server is the VPS-side exit of the Vector 1 tunnel.
//
// It listens for DTLS over UDP (the packets the VK TURN relay forwards to it), runs
// a KCP+smux session over each DTLS connection, and for every stream reads a target
// "host:port" header and pipes the stream to a freshly dialed TCP connection. That
// makes the VPS the exit node: the client's local SOCKS5 traffic rides the tunnel
// and reaches the open internet from here.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"time"

	"github.com/pion/dtls/v3"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/mux"
	"github.com/pristupaanastasia/turnvpn/internal/tunnel"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:56000", "UDP address to listen on for DTLS")
	secret := flag.String("secret", "", "shared secret for mutual DTLS auth (or env TURNVPN_SECRET); REQUIRED")
	flag.Parse()

	if *secret == "" {
		*secret = os.Getenv("TURNVPN_SECRET")
	}
	if *secret == "" {
		log.Fatal("server: -secret (or env TURNVPN_SECRET) is required — it authenticates clients so the exit is not an open proxy")
	}

	laddr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatalf("server: resolve %q: %v", *listen, err)
	}

	// Mutual PSK authentication: only a client holding the same secret can complete
	// the DTLS handshake, so the exit is not an open proxy and cannot be MITM'd.
	ln, err := dtls.ListenWithOptions("udp", laddr,
		dtls.WithPSK(authdtls.PSK(*secret)),
		dtls.WithPSKIdentityHint(authdtls.IdentityHint()),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	)
	if err != nil {
		log.Fatalf("server: dtls listen on %s: %v", *listen, err)
	}
	defer func() { _ = ln.Close() }()
	log.Printf("server: tunnel exit listening on %s", *listen)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("server: accept: %v", err)
			continue
		}
		go serve(conn)
	}
}

// serve completes the DTLS handshake, runs a smux session over it, and handles each
// stream as a new outbound TCP connection.
func serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	ra := conn.RemoteAddr()

	if dc, ok := conn.(*dtls.Conn); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := dc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			log.Printf("server: %s handshake: %v", ra, err)
			return
		}
	}
	log.Printf("server: tunnel up from %s", ra)

	sess, lane, err := mux.Server(conn)
	if err != nil {
		log.Printf("server: %s mux: %v", ra, err)
		return
	}
	defer func() { _ = sess.Close() }()

	tunnel.Serve(sess, lane, log.Printf)
	log.Printf("server: tunnel down from %s", ra)
}
