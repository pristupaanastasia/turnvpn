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
	"sync/atomic"
	"time"

	"github.com/bschaatsbergen/dnsdialer"
	"github.com/google/uuid"
	"github.com/pion/dtls/v3"
	"github.com/pion/logging"
	"github.com/pion/turn/v5"
	"github.com/pristupaanastasia/turnvpn/internal/captcha"
	"github.com/xtaci/smux"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/mux"
	"github.com/pristupaanastasia/turnvpn/internal/okapi"
	"github.com/pristupaanastasia/turnvpn/internal/socks"
	tun "github.com/pristupaanastasia/turnvpn/internal/tunnel"
)

type config struct {
	listen, peer, vkLink, token, appKey, secret, identity string
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
	peer := flag.String("peer", "", "VPS exit host:port (running `server`); comma-separated list rotates across exits")
	vkLink := flag.String("vk-link", "", "VK call join link token (from vk.ru/call/join/<token>)")
	token := flag.String("token", "", "VK per-call guest token (a.token); or env TURN_CALL_TOKEN")
	appKey := flag.String("app-key", "CGMMEJLGDIHBABABA", "OK application_key (public web-client key)")
	secret := flag.String("secret", "", "shared secret for mutual DTLS auth, must match the server (or env TURNVPN_SECRET); REQUIRED")
	identity := flag.String("identity", "", "client PSK identity for a server keyring (optional; empty = single-secret mode)")
	tunnels := flag.Int("tunnels", 2, "number of parallel tunnels (streams are round-robined across them for throughput)")
	refresh := flag.Duration("refresh", tunnelTTL, "proactively refresh each tunnel's session after this interval (before its ~10m creds expire)")
	flag.Parse()

	if *tunnels < 1 {
		*tunnels = 1
	}

	if *token == "" {
		*token = os.Getenv("TURN_CALL_TOKEN")
	}
	if *secret == "" {
		*secret = os.Getenv("TURNVPN_SECRET")
	}
	if *peer == "" || *vkLink == "" {
		log.Fatal("usage: client -peer vps:port -vk-link <link> -secret <shared> [-token <call-token>] [-tunnels N] [-listen 127.0.0.1:1080]")
	}
	if *secret == "" {
		log.Fatal("client: -secret (or env TURNVPN_SECRET) is required and must match the server")
	}
	cfg := config{listen: *listen, peer: *peer, vkLink: *vkLink, token: *token, appKey: *appKey, secret: *secret, identity: *identity}

	var peerAddrs []*net.UDPAddr
	for _, p := range strings.Split(cfg.peer, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		a, err := net.ResolveUDPAddr("udp4", p)
		if err != nil {
			log.Fatalf("client: resolve peer %q: %v", p, err)
		}
		peerAddrs = append(peerAddrs, a)
	}
	if len(peerAddrs) == 0 {
		log.Fatal("client: -peer is required (one or more VPS exits, comma-separated)")
	}
	// Credential source. An explicit -token uses the human-in-the-loop path (derive
	// turn_server from a browser-captured call token; no captcha solving). Otherwise
	// fall back to the pluggable solver wired here.
	var creds CredsFunc
	if cfg.token != "" {
		creds = defaultCreds(cfg)
	} else {
		dialer := dnsdialer.New(
			dnsdialer.WithResolvers("77.88.8.8:53", "77.88.8.1:53", "8.8.8.8:53", "8.8.4.4:53", "1.1.1.1:53", "1.0.0.1:53"),
			dnsdialer.WithStrategy(dnsdialer.Fallback{}),
			dnsdialer.WithCache(100, 10*time.Hour, 10*time.Hour),
		)
		creds = func(ctx context.Context, s string, streamID int) (string, string, string, error) {
			return captcha.GetVkCredsCached(ctx, s, streamID, dialer)
		}
	}
	p := newPool(cfg, peerAddrs, creds, *tunnels, *refresh)
	defer p.close()

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatalf("client: SOCKS5 listen on %s: %v", cfg.listen, err)
	}
	defer func() { _ = ln.Close() }()
	log.Printf("client: SOCKS5 on %s  ->  %d tunnel(s)  ->  %s (exit)", cfg.listen, *tunnels, cfg.peer)

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("client: accept: %v", err)
			continue
		}
		go serveLocal(p.pick(), c)
	}
}

// pool holds several tunnels and hands them out round-robin, so concurrent SOCKS
// connections spread across independent TURN allocations (more throughput, and one
// dead tunnel doesn't stall the others). Each tunnel runs its own background manager
// that keeps it alive and proactively refreshes its credentials.
type pool struct {
	tunnels []*tunnel
	next    atomic.Uint32
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newPool(cfg config, peerAddrs []*net.UDPAddr, creds CredsFunc, n int, ttl time.Duration) *pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pool{cancel: cancel, tunnels: make([]*tunnel, n)}
	for i := range p.tunnels {
		// Spread tunnels across the available exits; each re-establish rotates on.
		t := &tunnel{cfg: cfg, peers: peerAddrs, peerIdx: i, creds: creds, streamID: i, ttl: ttl, ready: make(chan struct{})}
		p.tunnels[i] = t
		p.wg.Add(1)
		go func(t *tunnel) { defer p.wg.Done(); t.run(ctx) }(t)
	}
	return p
}

// pick returns the next tunnel round-robin.
func (p *pool) pick() *tunnel {
	i := p.next.Add(1) - 1
	return p.tunnels[int(i)%len(p.tunnels)]
}

func (p *pool) close() {
	p.cancel()
	p.wg.Wait()
}

// serveLocal handles one local SOCKS5 connection: negotiate the target, open a
// tunnel stream, send the target header, and pipe.
func serveLocal(t *tunnel, c net.Conn) {
	defer func() { _ = c.Close() }()

	cmd, target, err := socks.Request(c)
	if err != nil {
		log.Printf("client: socks: %v", err)
		return
	}
	ref, err := t.getRef()
	if err != nil {
		log.Printf("client: tunnel unavailable: %v", err)
		return
	}

	switch cmd {
	case socks.CmdConnect:
		if err := socks.ReplyConnect(c); err != nil {
			return
		}
		stream, err := tun.Open(ref.sess, target)
		if err != nil {
			// Session likely just died; its manager goroutine will reconnect it.
			log.Printf("client: open stream for %s: %v", target, err)
			return
		}
		defer func() { _ = stream.Close() }()
		tun.Pipe(c, stream)
	case socks.CmdUDPAssociate:
		serveUDPAssociate(ref.udp, c)
	default:
		log.Printf("client: unsupported SOCKS command %d", cmd)
	}
}

// serveUDPAssociate runs one SOCKS5 UDP ASSOCIATE: it binds a local UDP relay, tells
// the client its address, and bridges the client's encapsulated datagrams to tunnel
// UDP flows (one per target) and the replies back. The association lives until the
// control TCP connection c closes.
func serveUDPAssociate(udp *tun.UDPFlows, c net.Conn) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		log.Printf("client: udp associate bind: %v", err)
		return
	}
	defer func() { _ = pc.Close() }()
	if err := socks.ReplyUDPAssociate(c, pc.LocalAddr().(*net.UDPAddr)); err != nil {
		return
	}
	// When the control connection closes, tear the association down.
	go func() { _, _ = io.Copy(io.Discard, c); _ = pc.Close() }()

	var mu sync.Mutex
	var clientAddr *net.UDPAddr
	flows := make(map[string]*tun.ClientUDPFlow)
	defer func() {
		mu.Lock()
		for _, f := range flows {
			_ = f.Close()
		}
		mu.Unlock()
	}()

	buf := make([]byte, 64*1024)
	for {
		n, addr, err := pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		target, payload, err := socks.DecodeUDP(buf[:n])
		if err != nil {
			continue
		}
		mu.Lock()
		clientAddr = addr
		f := flows[target]
		if f == nil {
			f, err = udp.Open(target)
			if err != nil {
				mu.Unlock()
				continue
			}
			flows[target] = f
			// Pump replies from this flow back to the client, SOCKS-encapsulated.
			go func(f *tun.ClientUDPFlow, target string) {
				for {
					select {
					case p := <-f.Inbound():
						out, err := socks.EncodeUDP(target, p)
						if err != nil {
							continue
						}
						mu.Lock()
						ca := clientAddr
						mu.Unlock()
						if ca != nil {
							_, _ = pc.WriteToUDP(out, ca)
						}
					case <-f.Done():
						return
					}
				}
			}(f, target)
		}
		mu.Unlock()
		_ = f.Send(payload)
	}
}

// Tunnel lifetime tuning. TURN credentials live ~10 min, so refresh well before
// that; keep a replaced session alive briefly so in-flight streams can finish.
const (
	tunnelTTL   = 8 * time.Minute
	retireGrace = 90 * time.Second
)

// sessRef bundles a live smux session with the resources that must be closed to tear
// the whole tunnel down (TURN client, relay allocation, DTLS conn).
type sessRef struct {
	sess    *smux.Session
	udp     *tun.UDPFlows // UDP flows (SOCKS UDP ASSOCIATE) over this session's lane
	closers []io.Closer
}

func (r *sessRef) teardown() {
	if r == nil {
		return
	}
	if r.sess != nil {
		_ = r.sess.Close()
	}
	for i := len(r.closers) - 1; i >= 0; i-- {
		_ = r.closers[i].Close()
	}
}

// tunnel is a single DTLS/KCP/smux session over one TURN allocation, kept alive by a
// background run() goroutine that proactively refreshes it before its credentials
// expire. A pool holds several. get() hands out the current live session.
type tunnel struct {
	cfg      config
	peers    []*net.UDPAddr // exit(s); rotated across on each (re)establish
	peerIdx  int            // next exit to use (only touched by run()/establish())
	creds    CredsFunc      // pluggable credential acquisition; see CredsFunc
	streamID int            // identifies this tunnel to creds (for cred-cache sharding)
	ttl      time.Duration  // proactively refresh the session after this long

	mu        sync.RWMutex
	cur       *sessRef
	ready     chan struct{} // closed once cur is first set
	readyOnce sync.Once
}

// run maintains the tunnel for ctx's lifetime: (re)establish a session, serve it
// until the refresh TTL elapses or it dies, then replace it — retiring the previous
// session after a grace period so active streams are not cut mid-transfer.
func (t *tunnel) run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		sess, lane, closers, err := t.establish()
		if err != nil {
			log.Printf("client: tunnel %d establish: %v (retry in %s)", t.streamID, err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = minDur(2*backoff, 30*time.Second)
			continue
		}
		backoff = time.Second
		ref := &sessRef{sess: sess, udp: tun.NewUDPFlows(sess, lane), closers: closers}
		t.setCur(ref)
		log.Printf("client: tunnel %d up (relay %s)", t.streamID, sess.LocalAddr())

		timer := time.NewTimer(t.ttl)
		select {
		case <-ctx.Done():
			timer.Stop()
			ref.teardown()
			return
		case <-timer.C:
			// Proactive refresh: the current session is still healthy, so retire it
			// with grace while the loop brings up a fresh one.
			go retire(ref, retireGrace)
		case <-sess.CloseChan():
			timer.Stop()
			log.Printf("client: tunnel %d dropped, reconnecting", t.streamID)
			go retire(ref, 0) // already dead; just release resources
		}
	}
}

// setCur swaps in a new current session and signals readiness on the first one.
func (t *tunnel) setCur(ref *sessRef) {
	t.mu.Lock()
	t.cur = ref
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.ready) })
}

// getRef returns the current live session ref (session + UDP flows), waiting for the
// first establish if needed.
func (t *tunnel) getRef() (*sessRef, error) {
	select {
	case <-t.ready:
	case <-time.After(45 * time.Second):
		return nil, fmt.Errorf("tunnel %d not ready", t.streamID)
	}
	t.mu.RLock()
	cur := t.cur
	t.mu.RUnlock()
	if cur == nil || cur.sess.IsClosed() {
		return nil, fmt.Errorf("tunnel %d unavailable", t.streamID)
	}
	return cur, nil
}

// retire closes a replaced session, but first lets its in-flight streams drain for up
// to grace (checking a few times a second). A grace of 0 tears down immediately.
func retire(ref *sessRef, grace time.Duration) {
	deadline := time.Now().Add(grace)
	for grace > 0 && time.Now().Before(deadline) {
		if ref.sess.IsClosed() || ref.sess.NumStreams() == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	ref.teardown()
}

// establish resolves fresh TURN creds (via the pluggable t.creds), allocates a relay,
// DTLS-handshakes through it to the VPS, and returns a smux client session plus the
// resources to close in LIFO.
func (t *tunnel) establish() (*smux.Session, *mux.Lane, []io.Closer, error) {
	cfg := t.cfg
	// Rotate to the next exit so a blocked or dead VPS is avoided on reconnect.
	peerAddr := t.peers[t.peerIdx%len(t.peers)]
	t.peerIdx++
	if len(t.peers) > 1 {
		log.Printf("client: tunnel %d exit -> %s", t.streamID, peerAddr)
	}
	// Resolving creds includes a person solving the captcha (up to 3 minutes).
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	user, pass, turnAddr, err := t.creds(ctx, cfg.vkLink, t.streamID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve creds: %w", err)
	}
	log.Printf("client: got turn_server %s", turnAddr)

	udp, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, nil, nil, err
	}
	closers := []io.Closer{udp}
	fail := func(e error) (*smux.Session, *mux.Lane, []io.Closer, error) {
		closeAll(closers)
		return nil, nil, nil, e
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

	// Mutual PSK authentication: the handshake only completes if the server holds
	// the same secret, which authenticates the exit (no MITM) and keeps forward
	// secrecy via the ECDHE-PSK suite.
	pskIdentity := authdtls.IdentityHint()
	if cfg.identity != "" {
		pskIdentity = []byte(cfg.identity)
	}
	dconn, err := dtls.ClientWithOptions(relay, peerAddr,
		dtls.WithPSK(authdtls.PSK(cfg.secret)),
		dtls.WithPSKIdentityHint(pskIdentity),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
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

	sess, lane, err := mux.Client(dconn)
	if err != nil {
		return fail(err)
	}
	log.Printf("client: tunnel established (relay %s)", relay.LocalAddr())
	return sess, lane, closers, nil
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

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func closeAll(cs []io.Closer) {
	for i := len(cs) - 1; i >= 0; i-- {
		_ = cs[i].Close()
	}
}

// sleepCtx waits for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
