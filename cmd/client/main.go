// Command client is the computer-side entry of the Vector 1 tunnel.
//
// For Stage 0 (the make-or-break relay test) it is a probe: it allocates a relay on
// a VK TURN server, opens a DTLS connection THROUGH that relay to an external peer
// (the VPS running `server`), sends a marker and checks it echoes back. A successful
// round-trip proves the VK TURN relay forwards DTLS-wrapped data to an arbitrary
// external peer — the premise the whole approach rests on, and the thing an earlier
// raw-UDP probe (cmd/probe) wrongly reported as dead because it sent unwrapped bytes.
//
// TURN credentials come from the VK call join descriptor's turn_server
// {username, credential, urls} (obtained out of band; capture is human-in-the-loop
// because the call-token step is captcha-gated). Pass them with -turn/-turn-user/
// -turn-pass. Later stages grow this into a local SOCKS5 proxy over the tunnel.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"github.com/pion/turn/v5"

	"github.com/pristupaanastasia/turnvpn/internal/okapi"
)

func main() {
	turnAddr := flag.String("turn", "", "TURN server host:port; if empty, fetched from -vk-link + -token")
	user := flag.String("turn-user", "", "TURN username (turn_server.username)")
	pass := flag.String("turn-pass", "", "TURN credential (turn_server.credential); or env TURN_PASS")
	vkLink := flag.String("vk-link", "", "VK call join link token (from vk.ru/call/join/<token>) to fetch TURN creds")
	token := flag.String("token", "", "VK per-call guest token (a.token); with -vk-link fetches turn_server (or env TURN_CALL_TOKEN)")
	appKey := flag.String("app-key", "CGMMEJLGDIHBABABA", "OK application_key (public web-client key)")
	peer := flag.String("peer", "", "external echo peer host:port (the VPS running `server`)")
	msg := flag.String("msg", "turnvpn-stage0-probe", "marker payload to send and expect echoed")
	flag.Parse()

	if *pass == "" {
		*pass = os.Getenv("TURN_PASS")
	}
	if *token == "" {
		*token = os.Getenv("TURN_CALL_TOKEN")
	}
	if *peer == "" {
		log.Fatal("usage: client -peer vps:port (-turn host:port -turn-user U -turn-pass P | -vk-link <link> -token <call-token>)")
	}

	ctx0 := context.Background()
	// If explicit TURN creds were not given, fetch turn_server from the VK call:
	// anonymLogin (no token) -> joinConversationByLink(anonymToken=token) -> turn_server.
	if *turnAddr == "" {
		if *vkLink == "" || *token == "" {
			log.Fatal("no TURN creds: pass -turn/-turn-user/-turn-pass, or -vk-link + -token")
		}
		ta, u, p, err := fetchTurnCreds(ctx0, *appKey, *vkLink, *token)
		if err != nil {
			log.Fatalf("client: fetch turn creds: %v", err)
		}
		*turnAddr, *user, *pass = ta, u, p
		log.Printf("client: got turn_server %s (from VK join)", ta)
	}
	if *turnAddr == "" || *user == "" || *pass == "" {
		log.Fatal("client: incomplete TURN creds")
	}

	// Local UDP socket the TURN client uses to reach the relay.
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		log.Fatalf("client: listen udp: %v", err)
	}
	defer func() { _ = conn.Close() }()

	tc, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: *turnAddr,
		Username:       *user,
		Password:       *pass,
		Conn:           conn,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		log.Fatalf("client: new turn client: %v", err)
	}
	defer tc.Close()
	if err := tc.Listen(); err != nil {
		log.Fatalf("client: turn listen: %v", err)
	}

	relayConn, err := tc.Allocate()
	if err != nil {
		log.Fatalf("client: allocate on %s: %v", *turnAddr, err)
	}
	defer func() { _ = relayConn.Close() }()
	log.Printf("client: allocated relay %s via %s", relayConn.LocalAddr(), *turnAddr)

	peerAddr, err := net.ResolveUDPAddr("udp4", *peer)
	if err != nil {
		log.Fatalf("client: resolve peer %q: %v", *peer, err)
	}
	if err := tc.CreatePermission(peerAddr); err != nil {
		log.Fatalf("client: create permission for %s: %v", peerAddr, err)
	}
	log.Printf("client: permission created for peer %s", peerAddr)

	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		log.Fatalf("client: self-signed cert: %v", err)
	}

	// The DTLS handshake runs THROUGH the relay: records are written with
	// relayConn.WriteTo(.., peerAddr) -> relay -> VPS, replies come back the same
	// way. If the relay drops non-media/raw payloads this hangs (the old false-
	// negative symptom); if it completes, DTLS-wrapped data crosses the relay.
	dconn, err := dtls.ClientWithOptions(relayConn, peerAddr,
		dtls.WithCertificates(cert),
		dtls.WithInsecureSkipVerify(true),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
	)
	if err != nil {
		log.Fatalf("client: dtls client setup: %v", err)
	}
	defer func() { _ = dconn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := dconn.HandshakeContext(ctx); err != nil {
		log.Fatalf("client: DTLS handshake through relay FAILED: %v\n"+
			"(Stage 0 negative: the relay did not pass the DTLS handshake to the peer)", err)
	}
	log.Printf("client: DTLS handshake OK through relay — testing echo")

	if _, err := dconn.Write([]byte(*msg)); err != nil {
		log.Fatalf("client: write: %v", err)
	}
	_ = dconn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64*1024)
	n, err := dconn.Read(buf)
	if err != nil {
		log.Fatalf("client: read echo: %v\n(handshake crossed but data did not round-trip)", err)
	}
	if got := string(buf[:n]); got != *msg {
		log.Fatalf("client: STAGE 0 MISMATCH: sent %q, got %q", *msg, got)
	}
	log.Printf("client: STAGE 0 PASS ✅ — %q echoed back through VK relay + DTLS", *msg)
}

// fetchTurnCreds runs the minimal OK join to extract the call's TURN credentials:
// anonymLogin (anonymous, no token) then joinConversationByLink with the per-call
// anonymToken. It returns the first TURN server as host:port plus its short-lived
// username/credential.
func fetchTurnCreds(ctx context.Context, appKey, link, token string) (addr, user, pass string, err error) {
	cl := &okapi.Client{AppKey: appKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
	if _, err = cl.AnonymLogin(ctx, uuid.NewString()); err != nil {
		return "", "", "", fmt.Errorf("anonymLogin: %w", err)
	}
	desc, err := cl.JoinConversationByLink(ctx, link, okapi.JoinOptions{AnonymToken: token})
	if err != nil {
		return "", "", "", fmt.Errorf("joinConversationByLink: %w", err)
	}
	ts := desc.TurnServer
	if len(ts.URLs) == 0 || ts.Username == "" || ts.Credential == "" {
		return "", "", "", fmt.Errorf("descriptor carried no turn_server credentials")
	}
	return turnHostPort(ts.URLs[0]), ts.Username, ts.Credential, nil
}

// turnHostPort reduces a TURN URL ("turn:host:port?transport=udp") to "host:port".
func turnHostPort(u string) string {
	u = strings.SplitN(u, "?", 2)[0]
	u = strings.TrimPrefix(u, "turns:")
	u = strings.TrimPrefix(u, "turn:")
	return u
}
