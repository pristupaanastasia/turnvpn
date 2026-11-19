// Package carrier establishes the DTLS-over-TURN transport that the WireGuard data
// plane rides on: it fetches a VK call's TURN credentials, allocates a relay, and
// completes a mutually-authenticated PSK-DTLS handshake to the VPS exit. The result
// is a single datagram-preserving net.Conn (the DTLS conn) that wgbind carries
// WireGuard over. It is platform-independent, so both the desktop client and the
// Android binding can share it.
package carrier

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pion/dtls/v3"
	"github.com/pion/logging"
	"github.com/pion/turn/v5"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/okapi"
)

// Creds resolves a call's TURN credentials: username, password and the TURN server
// "host:port".
type Creds func(ctx context.Context) (user, pass, addr string, err error)

// Config configures a carrier dial.
type Config struct {
	Peer     *net.UDPAddr // the VPS exit, through the TURN relay
	Secret   string       // DTLS PSK, must match the server
	Identity string       // PSK identity for a server keyring ("" = single-secret)
	Creds    Creds
	Logf     func(string, ...any)
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func closeAll(cs []io.Closer) {
	for i := len(cs) - 1; i >= 0; i-- {
		_ = cs[i].Close()
	}
}

// Dial brings the carrier up and returns the DTLS conn plus the closers that tear
// the whole transport (TURN client, relay allocation, DTLS conn) down.
func Dial(ctx context.Context, cfg Config) (net.Conn, []io.Closer, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	user, pass, turnAddr, err := cfg.Creds(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("carrier: resolve creds: %w", err)
	}
	cfg.Logf("carrier: got turn_server %s", turnAddr)

	udp, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, nil, err
	}
	closers := []io.Closer{udp}
	fail := func(e error) (net.Conn, []io.Closer, error) {
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

	if err := tc.CreatePermission(cfg.Peer); err != nil {
		return fail(fmt.Errorf("permission: %w", err))
	}

	// Mutual PSK authentication: the handshake completes only if the server holds the
	// same secret, authenticating the exit (no MITM) with forward secrecy (ECDHE-PSK).
	pskIdentity := authdtls.IdentityHint()
	if cfg.Identity != "" {
		pskIdentity = []byte(cfg.Identity)
	}
	dconn, err := dtls.ClientWithOptions(relay, cfg.Peer,
		dtls.WithPSK(authdtls.PSK(cfg.Secret)),
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
	cfg.Logf("carrier: tunnel established (relay %s)", relay.LocalAddr())
	return dconn, closers, nil
}

// TokenCreds returns a Creds that derives turn_server from a VK call using a call
// token obtained out of band (human-in-the-loop; no captcha solving here).
func TokenCreds(appKey, link, token string) Creds {
	return func(ctx context.Context) (user, pass, addr string, err error) {
		// fetchTurnCreds returns (addr, user, pass); Creds yields (user, pass, addr).
		addr, user, pass, err = fetchTurnCreds(ctx, appKey, link, token)
		return user, pass, addr, err
	}
}

// fetchTurnCreds runs the minimal OK join to extract the call's TURN credentials.
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
