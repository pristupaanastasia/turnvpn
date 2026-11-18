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
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/xtaci/smux"

	"github.com/pristupaanastasia/turnvpn/internal/mux"
	"github.com/pristupaanastasia/turnvpn/internal/socks"
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

	sess, err := mux.Server(conn)
	if err != nil {
		log.Printf("server: %s mux: %v", ra, err)
		return
	}
	defer func() { _ = sess.Close() }()

	var wg sync.WaitGroup
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			break // session closed
		}
		wg.Add(1)
		go func(s *smux.Stream) {
			defer wg.Done()
			handleStream(s)
		}(stream)
	}
	wg.Wait()
	log.Printf("server: tunnel down from %s", ra)
}

// handleStream reads the stream's target header and pipes it to a dialed TCP peer.
func handleStream(s *smux.Stream) {
	defer func() { _ = s.Close() }()

	target, err := socks.ReadTarget(s)
	if err != nil {
		log.Printf("server: stream %d: read target: %v", s.ID(), err)
		return
	}
	remote, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("server: dial %s: %v", target, err)
		return
	}
	defer func() { _ = remote.Close() }()
	pipe(s, remote)
}

// pipe copies bytes both ways between a and b until either side ends, then unblocks
// the other direction by closing both.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
