// Package vpntun maintains a single tunnel to the VPS exit and exposes it as a
// Dialer: every Dial opens a new multiplexed stream to a target host:port, which the
// server dials on the open internet. It is the reusable core a tun2socks engine
// drives on mobile (and could back a SOCKS listener elsewhere): the transport is the
// same TURN + PSK-DTLS + KCP/smux stack as cmd/client, minus the local SOCKS5 server
// and the multi-tunnel pool.
package vpntun

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/dtls/v3"
	"github.com/pion/logging"
	"github.com/pion/turn/v5"
	"github.com/xtaci/smux"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/mux"
	"github.com/pristupaanastasia/turnvpn/internal/okapi"
	"github.com/pristupaanastasia/turnvpn/internal/tunnel"
)

// CredsFunc resolves the relay's TURN credentials (username, password, "host:port").
// It matches the client's seam so a captcha-solving or token-based provider plugs in.
type CredsFunc func(ctx context.Context) (user, pass, addr string, err error)

// Config parameters a tunnel. Secret must match the server; Creds supplies TURN
// credentials; PeerAddr is the VPS exit "host:port".
type Config struct {
	PeerAddr string
	Secret   string
	Creds    CredsFunc
	Logf     func(string, ...any)
}

// Dialer maintains one tunnel session and dials targets through it, re-establishing
// the session on demand when it dies (creds expire or the relay drops).
type Dialer struct {
	cfg      Config
	peerAddr *net.UDPAddr

	mu      sync.Mutex
	sess    *smux.Session
	closers []io.Closer
	closed  bool
}

// New validates cfg and returns a Dialer. The first Dial establishes the tunnel.
func New(cfg Config) (*Dialer, error) {
	if cfg.PeerAddr == "" || cfg.Secret == "" || cfg.Creds == nil {
		return nil, fmt.Errorf("vpntun: PeerAddr, Secret and Creds are required")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	pa, err := net.ResolveUDPAddr("udp4", cfg.PeerAddr)
	if err != nil {
		return nil, fmt.Errorf("vpntun: resolve peer %q: %w", cfg.PeerAddr, err)
	}
	return &Dialer{cfg: cfg, peerAddr: pa}, nil
}

// Dial opens a new stream to target ("host:port") through the tunnel. It satisfies
// the dialer shape tun2socks and net proxies expect (network is ignored; the server
// always dials TCP).
func (d *Dialer) Dial(ctx context.Context, network, target string) (net.Conn, error) {
	sess, err := d.session()
	if err != nil {
		return nil, err
	}
	stream, err := tunnel.Open(sess, target)
	if err != nil {
		d.markDead(sess)
		return nil, fmt.Errorf("vpntun: open stream to %s: %w", target, err)
	}
	return stream, nil
}

// DialUDPStream opens a tunnel stream that relays UDP datagrams to target; the
// caller frames each datagram with a uint16 length prefix (see mobile.udpConn) and
// the server dials UDP and pipes them both ways.
func (d *Dialer) DialUDPStream(target string) (net.Conn, error) {
	sess, err := d.session()
	if err != nil {
		return nil, err
	}
	stream, err := tunnel.OpenUDP(sess, target)
	if err != nil {
		d.markDead(sess)
		return nil, fmt.Errorf("vpntun: open udp stream to %s: %w", target, err)
	}
	return stream, nil
}

// Connect eagerly establishes the tunnel (creds, relay, PSK-DTLS, mux) so that
// setup failures surface immediately instead of on the first Dial.
func (d *Dialer) Connect() error {
	_, err := d.session()
	return err
}

// session returns a live session, establishing or re-establishing as needed.
func (d *Dialer) session() (*smux.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, fmt.Errorf("vpntun: closed")
	}
	if d.sess != nil && !d.sess.IsClosed() {
		return d.sess, nil
	}
	d.teardownLocked()
	sess, closers, err := d.establish()
	if err != nil {
		return nil, err
	}
	d.sess, d.closers = sess, closers
	return sess, nil
}

func (d *Dialer) markDead(sess *smux.Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == sess && d.sess != nil {
		_ = d.sess.Close()
	}
}

// establish fetches creds, allocates a relay, PSK-DTLS-handshakes through it to the
// VPS, and layers mux. Resources are returned for LIFO teardown.
func (d *Dialer) establish() (*smux.Session, []io.Closer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	user, pass, turnAddr, err := d.cfg.Creds(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve creds: %w", err)
	}
	d.cfg.Logf("vpntun: got turn_server %s", turnAddr)

	udp, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, nil, err
	}
	closers := []io.Closer{udp}
	fail := func(e error) (*smux.Session, []io.Closer, error) {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
		return nil, nil, e
	}

	tc, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: turnAddr, Username: user, Password: pass,
		Conn: udp, LoggerFactory: logging.NewDefaultLoggerFactory(),
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
	if err := tc.CreatePermission(d.peerAddr); err != nil {
		return fail(fmt.Errorf("permission: %w", err))
	}

	dconn, err := dtls.ClientWithOptions(relay, d.peerAddr,
		dtls.WithPSK(authdtls.PSK(d.cfg.Secret)),
		dtls.WithPSKIdentityHint(authdtls.IdentityHint()),
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

	sess, err := mux.Client(dconn)
	if err != nil {
		return fail(err)
	}
	d.cfg.Logf("vpntun: tunnel established")
	return sess, closers, nil
}

func (d *Dialer) teardownLocked() {
	if d.sess != nil {
		_ = d.sess.Close()
		d.sess = nil
	}
	for i := len(d.closers) - 1; i >= 0; i-- {
		_ = d.closers[i].Close()
	}
	d.closers = nil
}

// Close tears the tunnel down; the Dialer is unusable afterwards.
func (d *Dialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	d.teardownLocked()
	return nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// TokenCreds returns a CredsFunc that resolves turn_server from a VK call using a
// per-call token captured out of band (anonymLogin + joinConversationByLink). This
// is the human-in-the-loop path: the token comes from a browser that passed VK's
// captcha.
func TokenCreds(appKey, link, token string) CredsFunc {
	return func(ctx context.Context) (user, pass, addr string, err error) {
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
		return ts.Username, ts.Credential, turnHostPort(ts.URLs[0]), nil
	}
}

// turnHostPort reduces a TURN URL ("turn:host:port?transport=udp") to "host:port".
func turnHostPort(u string) string {
	u = strings.SplitN(u, "?", 2)[0]
	u = strings.TrimPrefix(u, "turns:")
	u = strings.TrimPrefix(u, "turn:")
	return u
}
