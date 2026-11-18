// Command server is the VPS-side exit of the Vector 1 tunnel.
//
// It listens for DTLS over UDP (the packets the VK TURN relay forwards to it), runs
// a KCP+smux session over each DTLS connection, and for every stream reads a target
// "host:port" header and pipes the stream to a freshly dialed TCP connection. That
// makes the VPS the exit node: the client's local SOCKS5 traffic rides the tunnel
// and reaches the open internet from here.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pion/dtls/v3"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/mux"
	"github.com/pristupaanastasia/turnvpn/internal/tunnel"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:56000", "UDP address to listen on for DTLS")
	secret := flag.String("secret", "", "single shared secret for mutual DTLS auth (or env TURNVPN_SECRET)")
	keyringPath := flag.String("keyring", "", "path to a per-client keyring file ('identity secret' per line; or env TURNVPN_KEYRING) — enables per-client auth with revocation")
	shape := flag.String("shape", "", "footprint-shape the download bitrate cap, e.g. 2mbit (empty = unlimited)")
	flag.Parse()

	shapeBytes, err := mux.ParseBitrate(*shape)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	if *secret == "" {
		*secret = os.Getenv("TURNVPN_SECRET")
	}
	if *keyringPath == "" {
		*keyringPath = os.Getenv("TURNVPN_KEYRING")
	}

	// Pick the authentication mode: a per-client keyring (revocable identities) takes
	// precedence; otherwise a single shared secret. One of them is required so the
	// exit is never an open proxy.
	var psk dtls.PSKCallback
	switch {
	case *keyringPath != "":
		kr, err := loadKeyring(*keyringPath)
		if err != nil {
			log.Fatalf("server: keyring %q: %v", *keyringPath, err)
		}
		log.Printf("server: per-client keyring loaded (%d identities)", len(kr))
		psk = kr.PSK()
	case *secret != "":
		psk = authdtls.PSK(*secret)
	default:
		log.Fatal("server: provide -secret/TURNVPN_SECRET or -keyring/TURNVPN_KEYRING — auth keeps the exit from being an open proxy")
	}

	laddr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatalf("server: resolve %q: %v", *listen, err)
	}

	// Mutual PSK authentication: only a client holding a valid secret/identity can
	// complete the DTLS handshake, so the exit is not an open proxy and cannot be MITM'd.
	ln, err := dtls.ListenWithOptions("udp", laddr,
		dtls.WithPSK(psk),
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
		go serve(conn, shapeBytes)
	}
}

// serve completes the DTLS handshake, runs a smux session over it, and handles each
// stream as a new outbound TCP connection.
func serve(conn net.Conn, shapeBytes int) {
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

	sess, lane, err := mux.Server(conn, mux.WithRate(shapeBytes))
	if err != nil {
		log.Printf("server: %s mux: %v", ra, err)
		return
	}
	defer func() { _ = sess.Close() }()

	tunnel.Serve(sess, lane, log.Printf)
	log.Printf("server: tunnel down from %s", ra)
}

// loadKeyring parses a keyring file: one "identity secret" per line, blank lines and
// '#' comments ignored. Revoking a client is removing its line and restarting.
func loadKeyring(path string) (authdtls.Keyring, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	secrets := make(map[string]string)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: expected 'identity secret'", n)
		}
		id := fields[0]
		if _, dup := secrets[id]; dup {
			return nil, fmt.Errorf("line %d: duplicate identity %q", n, id)
		}
		secrets[id] = fields[1]
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return nil, fmt.Errorf("keyring has no identities")
	}
	return authdtls.NewKeyring(secrets), nil
}
