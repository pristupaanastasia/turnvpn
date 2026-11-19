// Command client is the computer-side entry of the Vector 1 tunnel.
//
// It runs a local SOCKS5 proxy. Each accepted connection egresses through a
// WireGuard session carried inside a single DTLS connection that runs THROUGH a VK
// TURN relay allocation. The VPS `server` terminates WireGuard and forwards to the
// open internet, so the browser reaches it over whitelisted VK call infrastructure.
// WireGuard runs in userspace on a gVisor netstack, so the client needs no TUN
// device and no root; hostnames are resolved by DNS over the tunnel (remote DNS).
//
// TURN credentials are fetched from the VK call join descriptor (anonymLogin +
// joinConversationByLink with the per-call token). They are short-lived; the tunnel
// is proactively re-established before they expire, reusing the same call token.
// Obtaining that call token is human-in-the-loop (the call-token step is
// captcha-gated).
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

	"github.com/bschaatsbergen/dnsdialer"
	"github.com/google/uuid"
	"github.com/pion/dtls/v3"
	"github.com/pion/logging"
	"github.com/pion/turn/v5"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/captcha"
	"github.com/pristupaanastasia/turnvpn/internal/okapi"
	"github.com/pristupaanastasia/turnvpn/internal/socks"
	"github.com/pristupaanastasia/turnvpn/internal/wgclient"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

type config struct {
	listen, vkLink, token, appKey, secret, identity string
	wgPriv, wgServerPub, wgAddr                     string
	mtu                                             int
}

// CredsFunc resolves the call's TURN credentials for a (re)connect: given the join
// link and a connection id it returns username, password and the TURN server
// "host:port". This is the seam for pluggable credential acquisition — the default
// (defaultCreds) uses a human-captured call token, but any implementation matching
// this signature can be plugged in, including one that solves the captcha
// automatically (that solver is not part of turnvpn; it is supplied by the caller,
// e.g. from internal/captcha).
type CredsFunc func(ctx context.Context, link string, streamID int) (user, pass, addr string, err error)

// defaultCreds is the built-in CredsFunc: it derives turn_server from the VK call
// using a call token already obtained out of band (human-in-the-loop; no captcha
// solving here).
func defaultCreds(cfg config) CredsFunc {
	return func(ctx context.Context, link string, _ int) (user, pass, addr string, err error) {
		addr, user, pass, err = fetchTurnCreds(ctx, cfg.appKey, link, cfg.token)
		return user, pass, addr, err
	}
}

// tunnelTTL is how long before the ~10-minute TURN credentials expire that the
// tunnel is proactively re-established.
const tunnelTTL = 8 * time.Minute

func main() {
	listen := flag.String("listen", "127.0.0.1:1080", "local SOCKS5 listen address")
	peer := flag.String("peer", "", "VPS exit host:port (running `server`); comma-separated list rotates across exits")
	vkLink := flag.String("vk-link", "", "VK call join link token (from vk.ru/call/join/<token>)")
	token := flag.String("token", "", "VK per-call guest token (a.token); or env TURN_CALL_TOKEN")
	appKey := flag.String("app-key", "CGMMEJLGDIHBABABA", "OK application_key (public web-client key)")
	secret := flag.String("secret", "", "shared secret for mutual DTLS auth, must match the server (or env TURNVPN_SECRET); REQUIRED")
	identity := flag.String("identity", "", "client PSK identity for a server keyring (optional; empty = single-secret mode)")
	wgKeyHex := flag.String("wg-key", "", "client WireGuard private key (hex; or env TURNVPN_WG_KEY)")
	wgServerPub := flag.String("wg-server-pub", "", "server WireGuard public key (hex; or env TURNVPN_WG_SERVER_PUB)")
	wgAddr := flag.String("wg-addr", "10.6.0.2", "this client's WireGuard tunnel address")
	mtu := flag.Int("mtu", 1280, "inner tunnel MTU")
	refresh := flag.Duration("refresh", tunnelTTL, "re-establish the tunnel after this interval (before its ~10m creds expire)")
	genKey := flag.Bool("genkey", false, "print a fresh WireGuard keypair and exit")
	flag.Parse()

	if *genKey {
		priv, pub, err := wgkey.Generate()
		if err != nil {
			log.Fatalf("client: genkey: %v", err)
		}
		fmt.Printf("private_key (client — keep secret): %s\npublic_key  (add to server peers):   %s\n", priv, pub)
		return
	}

	if *token == "" {
		*token = os.Getenv("TURN_CALL_TOKEN")
	}
	if *secret == "" {
		*secret = os.Getenv("TURNVPN_SECRET")
	}
	if *wgKeyHex == "" {
		*wgKeyHex = os.Getenv("TURNVPN_WG_KEY")
	}
	if *wgServerPub == "" {
		*wgServerPub = os.Getenv("TURNVPN_WG_SERVER_PUB")
	}
	if *peer == "" || *vkLink == "" {
		log.Fatal("usage: client -peer vps:port -vk-link <link> -secret <shared> -wg-key <hex> -wg-server-pub <hex> [-token <call-token>] [-wg-addr 10.6.0.2] [-listen 127.0.0.1:1080]")
	}
	if *secret == "" {
		log.Fatal("client: -secret (or env TURNVPN_SECRET) is required and must match the server")
	}
	if *wgKeyHex == "" || *wgServerPub == "" {
		log.Fatal("client: -wg-key and -wg-server-pub are required (use -genkey to make a client keypair; add its public key to the server peers file)")
	}
	if _, err := wgkey.Public(*wgKeyHex); err != nil {
		log.Fatalf("client: -wg-key: %v", err)
	}

	cfg := config{
		listen: *listen, vkLink: *vkLink, token: *token, appKey: *appKey,
		secret: *secret, identity: *identity,
		wgPriv: *wgKeyHex, wgServerPub: *wgServerPub, wgAddr: *wgAddr, mtu: *mtu,
	}

	var peerAddrs []*net.UDPAddr
	for _, p := range strings.Split(*peer, ",") {
		if p = strings.TrimSpace(p); p == "" {
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

	// Credential source: an explicit -token uses the human-in-the-loop path;
	// otherwise the pluggable solver wired here.
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

	t := &tunnel{cfg: cfg, peers: peerAddrs, creds: creds, refresh: *refresh}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go t.run(ctx)

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatalf("client: SOCKS5 listen on %s: %v", cfg.listen, err)
	}
	defer func() { _ = ln.Close() }()
	log.Printf("client: SOCKS5 on %s  ->  WireGuard tunnel  ->  %s (exit)", cfg.listen, *peer)

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("client: accept: %v", err)
			continue
		}
		go serveConn(t, c)
	}
}

// clientRef bundles a live WireGuard client with the carrier resources that must be
// closed to tear the whole tunnel down (TURN client, relay allocation, DTLS conn).
type clientRef struct {
	client  *wgclient.Client
	closers []io.Closer
}

func (r *clientRef) teardown() {
	if r == nil {
		return
	}
	if r.client != nil {
		_ = r.client.Close()
	}
	closeAll(r.closers)
}

// tunnel owns the single WireGuard-over-DTLS-over-TURN session, kept alive by a
// background run() goroutine that proactively re-establishes it before the TURN
// credentials expire. get() hands out the current live client.
type tunnel struct {
	cfg     config
	peers   []*net.UDPAddr
	peerIdx int
	creds   CredsFunc
	refresh time.Duration

	mu  sync.Mutex
	cur *clientRef
}

func (t *tunnel) setCur(r *clientRef) {
	t.mu.Lock()
	t.cur = r
	t.mu.Unlock()
}

func (t *tunnel) get() (*wgclient.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cur == nil || t.cur.client == nil {
		return nil, fmt.Errorf("tunnel not ready")
	}
	return t.cur.client, nil
}

// run establishes the tunnel and re-establishes it every refresh interval,
// bringing the replacement up before tearing the old one down to minimise the gap.
func (t *tunnel) run(ctx context.Context) {
	var cur *clientRef
	backoff := time.Second
	for ctx.Err() == nil {
		ref, err := t.establish(ctx)
		if err != nil {
			log.Printf("client: establish failed: %v (retry in %s)", err, backoff)
			if !sleepCtx(ctx, backoff) {
				break
			}
			backoff = minDur(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		t.setCur(ref)
		log.Printf("client: tunnel ready")
		if cur != nil {
			cur.teardown() // old torn down only after the replacement is live
		}
		cur = ref
		if !sleepCtx(ctx, t.refresh) {
			break
		}
		log.Printf("client: refreshing tunnel before credentials expire")
	}
	t.setCur(nil)
	if cur != nil {
		cur.teardown()
	}
}

// establish gets fresh TURN creds, opens the DTLS carrier through a TURN allocation,
// and brings a WireGuard client up over it.
func (t *tunnel) establish(ctx context.Context) (*clientRef, error) {
	cfg := t.cfg
	peerAddr := t.peers[t.peerIdx%len(t.peers)]
	t.peerIdx++
	if len(t.peers) > 1 {
		log.Printf("client: exit -> %s", peerAddr)
	}

	// Resolving creds may include a person solving the captcha (up to ~3 minutes).
	cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	user, pass, turnAddr, err := t.creds(cctx, cfg.vkLink, 0)
	if err != nil {
		return nil, fmt.Errorf("resolve creds: %w", err)
	}
	log.Printf("client: got turn_server %s", turnAddr)

	udp, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	closers := []io.Closer{udp}
	fail := func(e error) (*clientRef, error) {
		closeAll(closers)
		return nil, e
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

	// Mutual PSK authentication: the handshake completes only if the server holds the
	// same secret, authenticating the exit (no MITM) with forward secrecy (ECDHE-PSK).
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
	hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
	err = dconn.HandshakeContext(hctx)
	hcancel()
	if err != nil {
		return fail(fmt.Errorf("dtls handshake: %w", err))
	}
	closers = append(closers, dconn)

	client, err := wgclient.New(dconn, wgclient.Config{
		PrivKey:   cfg.wgPriv,
		ServerPub: cfg.wgServerPub,
		Address:   cfg.wgAddr,
		MTU:       cfg.mtu,
		Logf:      log.Printf,
	})
	if err != nil {
		return fail(fmt.Errorf("wireguard: %w", err))
	}
	log.Printf("client: tunnel established (relay %s)", relay.LocalAddr())
	return &clientRef{client: client, closers: closers}, nil
}

// serveConn handles one local SOCKS5 connection: CONNECT dials the target through
// the tunnel and splices; UDP ASSOCIATE bridges datagrams to tunnel UDP flows.
func serveConn(t *tunnel, c net.Conn) {
	defer func() { _ = c.Close() }()

	cmd, target, err := socks.Request(c)
	if err != nil {
		log.Printf("client: socks: %v", err)
		return
	}
	client, err := t.get()
	if err != nil {
		log.Printf("client: tunnel unavailable: %v", err)
		return
	}

	switch cmd {
	case socks.CmdConnect:
		if err := socks.ReplyConnect(c); err != nil {
			return
		}
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := client.DialContext(dctx, "tcp", target)
		cancel()
		if err != nil {
			log.Printf("client: dial %s: %v", target, err)
			return
		}
		defer func() { _ = out.Close() }()
		pipe(c, out)
	case socks.CmdUDPAssociate:
		serveUDPAssociate(client, c)
	default:
		log.Printf("client: unsupported SOCKS command %d", cmd)
	}
}

// serveUDPAssociate runs one SOCKS5 UDP ASSOCIATE: it binds a local UDP relay, tells
// the client its address, and bridges the client's encapsulated datagrams to tunnel
// UDP connections (one per target) and the replies back. The association lives until
// the control TCP connection c closes.
func serveUDPAssociate(client *wgclient.Client, c net.Conn) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		log.Printf("client: udp associate bind: %v", err)
		return
	}
	defer func() { _ = pc.Close() }()
	if err := socks.ReplyUDPAssociate(c, pc.LocalAddr().(*net.UDPAddr)); err != nil {
		return
	}
	go func() { _, _ = io.Copy(io.Discard, c); _ = pc.Close() }()

	var mu sync.Mutex
	var clientAddr *net.UDPAddr
	conns := make(map[string]net.Conn)
	defer func() {
		mu.Lock()
		for _, u := range conns {
			_ = u.Close()
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
		u := conns[target]
		if u == nil {
			dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			u, err = client.DialContext(dctx, "udp", target)
			cancel()
			if err != nil {
				mu.Unlock()
				continue
			}
			conns[target] = u
			// Pump replies from this flow back to the client, SOCKS-encapsulated.
			go func(u net.Conn, target string) {
				rbuf := make([]byte, 64*1024)
				for {
					_ = u.SetReadDeadline(time.Now().Add(60 * time.Second))
					m, err := u.Read(rbuf)
					if m > 0 {
						if out, e := socks.EncodeUDP(target, rbuf[:m]); e == nil {
							mu.Lock()
							ca := clientAddr
							mu.Unlock()
							if ca != nil {
								_, _ = pc.WriteToUDP(out, ca)
							}
						}
					}
					if err != nil {
						return
					}
				}
			}(u, target)
		}
		mu.Unlock()
		_, _ = u.Write(payload)
	}
}

// pipe copies bidirectionally between two conns and returns when either closes.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
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
