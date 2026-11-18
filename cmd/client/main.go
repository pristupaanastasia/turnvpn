// Command client is the computer-side entry of the Vector 1 tunnel.
//
// It runs a local SOCKS5 proxy. Each accepted connection is carried, as one smux
// stream, over a single tunnel to the VPS: the tunnel is a KCP+smux session over a
// DTLS connection that runs THROUGH a VK TURN relay allocation. The VPS `server`
// dials the SOCKS target and pipes it back, so the browser reaches the open internet
// over whitelisted VK call infrastructure.
//
// TURN credentials are fetched from the VK call join descriptor (anonymLogin +
// joinConversationByLink with the per-call token). They are short-lived; when the
// tunnel drops (creds expire or the relay resets) the next request transparently
// re-establishes it with fresh creds, reusing the same call token. Obtaining that
// call token is human-in-the-loop (the call-token step is captcha-gated).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"github.com/pion/turn/v5"
	"github.com/xtaci/smux"

	"github.com/pristupaanastasia/turnvpn/internal/mux"
	"github.com/pristupaanastasia/turnvpn/internal/okapi"
	"github.com/pristupaanastasia/turnvpn/internal/socks"
)

type config struct {
	listen, peer, vkLink, token, appKey string
}

// CredsFunc resolves the call's TURN credentials for a (re)connect: given the join
// link and a stream/connection id it returns username, password and the TURN server
// "host:port". This is the seam for pluggable credential acquisition — the default
// (defaultCreds) uses a human-captured call token, but any implementation matching
// this signature can be assigned to tunnel.creds instead, including one that solves
// the captcha automatically (that solver is not part of turnvpn; it is supplied by
// the caller, e.g. from internal/captcha).
type CredsFunc func(ctx context.Context, link string, streamID int) (user, pass, addr string, err error)

// defaultCreds is the built-in CredsFunc: it derives turn_server from the VK call
// using a call token already obtained out of band (human-in-the-loop; no captcha
// solving here). streamID is ignored (one tunnel, no per-stream sharding).
func defaultCreds(cfg config) CredsFunc {
	return func(ctx context.Context, link string, _ int) (user, pass, addr string, err error) {
		addr, user, pass, err = fetchTurnCreds(ctx, cfg.appKey, link, cfg.token)
		return user, pass, addr, err
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:1080", "local SOCKS5 listen address")
	peer := flag.String("peer", "", "VPS exit host:port (running `server`)")
	vkLink := flag.String("vk-link", "", "VK call join link token (from vk.ru/call/join/<token>)")
	token := flag.String("token", "", "VK per-call guest token (a.token); or env TURN_CALL_TOKEN")
	appKey := flag.String("app-key", "CGMMEJLGDIHBABABA", "OK application_key (public web-client key)")
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("TURN_CALL_TOKEN")
	}
	if *peer == "" || *vkLink == "" || *token == "" {
		log.Fatal("usage: client -peer vps:port -vk-link <link> -token <call-token> [-listen 127.0.0.1:1080]")
	}
	cfg := config{listen: *listen, peer: *peer, vkLink: *vkLink, token: *token, appKey: *appKey}

	peerAddr, err := net.ResolveUDPAddr("udp4", cfg.peer)
	if err != nil {
		log.Fatalf("client: resolve peer %q: %v", cfg.peer, err)
	}

	t := &tunnel{cfg: cfg, peerAddr: peerAddr, creds: defaultCreds(cfg)}
	// To use automatic credential acquisition (e.g. a captcha-solving fetcher that
	// matches CredsFunc), assign it here instead:  t.creds = <yourCredsFunc>
	defer t.close()

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatalf("client: SOCKS5 listen on %s: %v", cfg.listen, err)
	}
	defer func() { _ = ln.Close() }()
	log.Printf("client: SOCKS5 on %s  ->  tunnel  ->  %s (exit)", cfg.listen, cfg.peer)

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("client: accept: %v", err)
			continue
		}
		go serveLocal(t, c)
	}
}

// serveLocal handles one local SOCKS5 connection: negotiate the target, open a
// tunnel stream, send the target header, and pipe.
func serveLocal(t *tunnel, c net.Conn) {
	defer func() { _ = c.Close() }()

	target, err := socks.Handshake(c)
	if err != nil {
		log.Printf("client: socks: %v", err)
		return
	}
	sess, err := t.get()
	if err != nil {
		log.Printf("client: tunnel unavailable: %v", err)
		return
	}
	stream, err := sess.OpenStream()
	if err != nil {
		t.markDead(sess) // force re-establish on the next request
		log.Printf("client: open stream: %v", err)
		return
	}
	defer func() { _ = stream.Close() }()
	if err := socks.WriteTarget(stream, target); err != nil {
		log.Printf("client: write target %s: %v", target, err)
		return
	}
	pipe(c, stream)
}

// tunnel holds the current smux session and lazily (re)establishes it.
type tunnel struct {
	cfg      config
	peerAddr *net.UDPAddr
	creds    CredsFunc // pluggable credential acquisition; see CredsFunc

	mu      sync.Mutex
	sess    *smux.Session
	closers []io.Closer
}

// get returns a live session, establishing or re-establishing it if needed.
func (t *tunnel) get() (*smux.Session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sess != nil && !t.sess.IsClosed() {
		return t.sess, nil
	}
	t.teardownLocked()
	sess, closers, err := t.establish()
	if err != nil {
		return nil, err
	}
	t.sess, t.closers = sess, closers
	return sess, nil
}

// markDead closes the session if it is still the current one, so get() rebuilds.
func (t *tunnel) markDead(sess *smux.Session) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sess == sess && t.sess != nil {
		_ = t.sess.Close()
	}
}

func (t *tunnel) teardownLocked() {
	if t.sess != nil {
		_ = t.sess.Close()
		t.sess = nil
	}
	for i := len(t.closers) - 1; i >= 0; i-- {
		_ = t.closers[i].Close()
	}
	t.closers = nil
}

func (t *tunnel) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.teardownLocked()
}

// establish resolves fresh TURN creds (via the pluggable t.creds), allocates a relay,
// DTLS-handshakes through it to the VPS, and returns a smux client session plus the
// resources to close in LIFO.
func (t *tunnel) establish() (*smux.Session, []io.Closer, error) {
	cfg, peerAddr := t.cfg, t.peerAddr
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	user, pass, turnAddr, err := t.creds(ctx, cfg.vkLink, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve creds: %w", err)
	}
	log.Printf("client: got turn_server %s", turnAddr)

	udp, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, nil, err
	}
	closers := []io.Closer{udp}
	fail := func(e error) (*smux.Session, []io.Closer, error) {
		closeAll(closers)
		return nil, nil, e
	}

	tc, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: turnAddr,
		Username:       user,
		Password:       pass,
		Conn:           udp,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		return fail(err)
	}
	if err := tc.Listen(); err != nil {
		tc.Close()
		return fail(err)
	}
	closers = append(closers, closerFunc(func() error { tc.Close(); return nil }))

	relay, err := tc.Allocate()
	if err != nil {
		return fail(fmt.Errorf("allocate: %w", err))
	}
	closers = append(closers, relay)

	if err := tc.CreatePermission(peerAddr); err != nil {
		return fail(fmt.Errorf("permission: %w", err))
	}

	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return fail(err)
	}
	dconn, err := dtls.ClientWithOptions(relay, peerAddr,
		dtls.WithCertificates(cert),
		dtls.WithInsecureSkipVerify(true),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
	)
	if err != nil {
		return fail(err)
	}
	hctx, hcancel := context.WithTimeout(context.Background(), 20*time.Second)
	err = dconn.HandshakeContext(hctx)
	hcancel()
	if err != nil {
		return fail(fmt.Errorf("dtls handshake: %w", err))
	}
	closers = append(closers, dconn)

	sess, err := mux.Client(dconn)
	if err != nil {
		return fail(err)
	}
	log.Printf("client: tunnel established (relay %s)", relay.LocalAddr())
	return sess, closers, nil
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

// pipe copies bytes both ways between a and b until either side ends.
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

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func closeAll(cs []io.Closer) {
	for i := len(cs) - 1; i >= 0; i-- {
		_ = cs[i].Close()
	}
}
