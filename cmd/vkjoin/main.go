// Command vkjoin drives the VK Звонки join end to end from the reversed stack:
// it authenticates (OK auth.anonymLogin), joins the conversation
// (vchat.joinConversationByLink), builds the signaling URL, dials it over
// WebTransport, and prints the signaling messages that arrive. It is the capture
// tool for the next reverse step — understanding the signaling message schema —
// the same capture→understand method used for the REST layer.
//
// A guest join needs TWO tokens from the same anonymous browser session: the
// anonymous access token (JWT) for auth.anonymLogin (-access-token / -auto-token),
// and the per-call token (a.token) for the join's anonymToken (-token). They are
// distinct: calls.getAnonymousToken (which mints the per-call token) is captcha-gated
// for headless clients, so pass a browser-captured -token. The CLI never prints its
// own tokens; captured signaling message content is logged quoted and length-bounded
// but is not itself redacted.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/graceful"
	"github.com/pristupaanastasia/turnvpn/internal/okapi"
	"github.com/pristupaanastasia/turnvpn/internal/vkapi"
	"github.com/pristupaanastasia/turnvpn/internal/vkcall"
	"github.com/pristupaanastasia/turnvpn/internal/wttransport"
)

// httpTimeout bounds each OK API request so a stalled server cannot hang the run.
const httpTimeout = 30 * time.Second

func main() {
	link := flag.String("link", "", "join link token from vk.ru/call/join/<token> (required)")
	token := flag.String("token", "", "VK per-call guest token (a.token) for the join's anonymToken; capture from the browser (or env VKJOIN_TOKEN). If empty it is derived from the access token (captcha-gated)")
	accessToken := flag.String("access-token", "", "VK anonymous access token (JWT, anonym.…) for anonymLogin; required unless -auto-token (or env VKJOIN_ACCESS_TOKEN). Must be from the same browser session as -token")
	autoToken := flag.Bool("auto-token", false, "fetch the anonymous access token (JWT) automatically via get_anonym_token (captcha-free)")
	name := flag.String("name", "probe", "guest display name")
	appKey := flag.String("app-key", "CGMMEJLGDIHBABABA", "OK application_key (public web-client key)")
	deviceID := flag.String("device-id", "turnvpn-vkjoin", "stable device id for anonymLogin")
	clientVer := flag.String("client-version", "0.0.0", "client_version for anonymLogin")
	entityType := flag.String("entity-type", "", "signaling entityType (from conversation params; may be required)")
	video := flag.Bool("video", false, "request a video session")
	webrtcMode := flag.Bool("webrtc", false, "actually join over WebRTC (answer the SFU) instead of only capturing signaling")
	dur := flag.Duration("duration", 30*time.Second, "how long to listen for signaling messages")
	flag.Parse()

	tok := *token
	if tok == "" {
		tok = os.Getenv("VKJOIN_TOKEN") // keep the token out of the process list
	}
	accTok := *accessToken
	if accTok == "" {
		accTok = os.Getenv("VKJOIN_ACCESS_TOKEN")
	}
	// anonymLogin always needs the access JWT; the call token is optional (it can be
	// derived, though that path is captcha-gated).
	if *link == "" || (accTok == "" && !*autoToken) {
		fmt.Fprintln(os.Stderr, "usage: vkjoin -link <token> (-access-token <jwt> | -auto-token) [-token <call-token>] [flags]")
		fmt.Fprintln(os.Stderr, "       anonymLogin needs the anonymous access token (JWT); the join needs the per-call token (-token).")
		fmt.Fprintln(os.Stderr, "       capture both from the same browser session; tokens may also come from VKJOIN_TOKEN / VKJOIN_ACCESS_TOKEN.")
		flag.PrintDefaults()
		os.Exit(2)
	}

	r := graceful.New()
	defer r.Stop()

	if err := run(r.Context(), config{
		link: *link, token: tok, accessToken: accTok, autoToken: *autoToken, name: *name,
		appKey: *appKey, deviceID: *deviceID, webrtc: *webrtcMode,
		clientVersion: *clientVer, entityType: *entityType, video: *video, duration: *dur,
	}); err != nil {
		log.Fatalf("vkjoin: %v", err)
	}
}

type config struct {
	link, token, accessToken, name              string
	appKey, deviceID, clientVersion, entityType string
	autoToken, video, webrtc                    bool
	duration                                    time.Duration
}

func run(ctx context.Context, cfg config) error {
	authToken, callToken, err := resolveTokens(ctx, cfg)
	if err != nil {
		return err
	}

	cl := &okapi.Client{AppKey: cfg.appKey, HTTP: &http.Client{Timeout: httpTimeout}}

	// anonymLogin authenticates the anonymous identity with the access JWT; the
	// per-call token is NOT accepted here (it is only the join's anonymToken).
	login, err := cl.AnonymLogin(ctx, authToken, cfg.deviceID, cfg.clientVersion)
	if err != nil {
		return fmt.Errorf("anonymLogin: %w", err)
	}
	log.Printf("anonymLogin ok (uid=%s, api=%s)", login.UID, cl.APIServer)

	desc, err := cl.JoinConversationByLink(ctx, cfg.link, okapi.JoinOptions{
		IsVideo:     cfg.video,
		AnonymToken: callToken,
	})
	if err != nil {
		return fmt.Errorf("joinConversationByLink: %w", err)
	}
	log.Printf("joined: endpoint=%s wt=%s turn=%v", desc.Endpoint, desc.WtEndpoint, desc.TurnServer.URLs)

	params, useWT, err := buildParams(login, desc, cfg)
	if err != nil {
		return err
	}
	if !useWT {
		// Dial only speaks WebTransport; a wss:// endpoint would just fail there.
		return fmt.Errorf("no wt_endpoint in descriptor; WebSocket fallback not implemented")
	}
	sigURL, err := wttransport.BuildSignalingURL(desc.WtEndpoint, params, true)
	if err != nil {
		return fmt.Errorf("build signaling url: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sess, err := wttransport.Dial(dialCtx, sigURL, wttransport.DialOptions{})
	if err != nil {
		return fmt.Errorf("dial signaling: %w", err)
	}
	defer sess.Close()
	log.Printf("signaling connected (compression=%d)", wttransport.CompressionFromURL(sigURL))

	if cfg.webrtc {
		// Actually join: answer the SFU, connect, and hold the call for -duration.
		callCtx, cancel := context.WithTimeout(ctx, cfg.duration)
		defer cancel()
		res, err := vkcall.RunCall(callCtx, sess, log.Printf)
		if res != nil && res.ConnectionInfo != nil {
			log.Printf("call %s: connected=%v", res.ConnectionInfo.Conversation.ID, res.Connected)
		}
		// A clean end is: we connected, then held the call to the -duration deadline
		// (or a Ctrl+C). Timing out *before* connect, or any other error, is a failure.
		if res != nil && res.Connected && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("join call: %w", err)
		}
		return nil
	}

	log.Printf("capturing signaling for %s", cfg.duration)
	deadline := time.After(cfg.duration)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline:
			log.Printf("listen window elapsed")
			return nil
		case msg, ok := <-sess.Messages():
			if !ok {
				if err := sess.Err(); err != nil {
					return fmt.Errorf("signaling closed: %w", err)
				}
				log.Printf("signaling closed cleanly")
				return nil
			}
			logMessage(msg)
		}
	}
}

// resolveTokens returns the two distinct tokens a guest join needs:
//
//   - authToken: the VK anonymous ACCESS token (JWT, "anonym.<...>"), used as the
//     auth.anonymLogin auth_token. From -access-token/env, or, with -auto-token,
//     fetched via get_anonym_token (captcha-free).
//   - callToken: the per-call guest token (a.token), used as joinConversationByLink's
//     anonymToken. From -token/env, or derived from the access token via
//     calls.getAnonymousToken (which is captcha-gated for headless clients, so the
//     human-in-the-loop path is to paste a browser-captured -token instead).
//
// The two must belong to the SAME anonymous identity: the call token is minted for
// the anonym_id inside the access JWT, and anonymLogin must present that same JWT, so
// a self-fetched JWT cannot be mixed with a browser-captured call token.
func resolveTokens(ctx context.Context, cfg config) (authToken, callToken string, err error) {
	vk := &vkapi.Client{HTTP: &http.Client{Timeout: httpTimeout}}

	authToken = cfg.accessToken
	// A self-fetched JWT (-auto-token) has a different anonym_id than a
	// browser-captured -token, so the join will be rejected. Warn loudly.
	if cfg.autoToken && cfg.accessToken == "" && cfg.token != "" {
		log.Printf("warning: -auto-token fetches a NEW anonymous identity but -token is from another session; the join will likely be rejected. Pass -access-token captured alongside -token instead")
	}
	if authToken == "" && cfg.autoToken {
		a, aerr := vk.GetAnonymToken(ctx)
		if aerr != nil {
			return "", "", fmt.Errorf("get anonymous access token: %w", aerr)
		}
		authToken = a
		log.Printf("obtained anonymous access token")
	}

	callToken = cfg.token
	if callToken == "" {
		// Derive it from the access token (captcha-gated). Needs the access JWT.
		if authToken == "" {
			return "", "", fmt.Errorf("no call token: pass -token, or -access-token/-auto-token to derive one")
		}
		// Retry politely on transient flood control; a captcha returns immediately
		// (it must be solved by a human in a browser, never retried away).
		tok, terr := vk.GetCallTokenRetry(ctx, authToken, cfg.link, cfg.name, vkapi.DefaultRetryPolicy)
		if terr != nil {
			return "", "", fmt.Errorf("get call token: %w", terr)
		}
		callToken = tok
		log.Printf("derived call token from access token")
	}

	// anonymLogin needs the access JWT; the call token alone will not authenticate
	// ("Access token is broken"). Require the JWT explicitly.
	if authToken == "" {
		return "", "", fmt.Errorf("anonymLogin needs the anonymous access token (JWT): pass -access-token (captured from the browser alongside -token) or -auto-token")
	}
	return authToken, callToken, nil
}

// buildParams maps the login + descriptor onto the signaling URL parameters, and
// reports whether to use the WebTransport endpoint (preferred when present).
func buildParams(login *okapi.AnonymLoginResult, desc *okapi.Descriptor, cfg config) (wttransport.SignalingParams, bool, error) {
	var uid int64
	if login.UID != "" {
		if v, err := strconv.ParseInt(login.UID, 10, 64); err == nil {
			uid = v
		} else {
			// Non-numeric uid is anomalous but not fatal; proceed with 0 and warn.
			log.Printf("warning: non-numeric uid %q from anonymLogin; using userId=0", login.UID)
		}
	}
	p := wttransport.SignalingParams{
		UserID:         uid,
		EntityType:     cfg.entityType,
		DeviceIdx:      desc.DeviceIdx,
		ConversationID: desc.ID,
		Token:          desc.Token,
		Capabilities:   okapi.DefaultCapabilities,
		ClientType:     okapi.WebClientType,
		AppVersion:     cfg.clientVersion,
		Device:         cfg.deviceID,
		Platform:       "web",
		UserAgent:      "turnvpn/vkjoin",
	}
	useWT := desc.WtEndpoint != ""
	return p, useWT, nil
}

// sensitiveField matches JSON fields whose values are secret (ephemeral TURN
// credentials / tokens) in a signaling message, so they can be redacted in logs.
var sensitiveField = regexp.MustCompile(`"(credential|username|token|secret)"\s*:\s*"[^"]*"`)

// logMessage prints a captured signaling message for analysis. Payloads may carry
// TURN credentials, tokens and SDP crypto, so it redacts known secret fields,
// then prints the length and a bounded, quoted preview — %q so control bytes or a
// truncated UTF-8 sequence cannot garble or inject the log.
func logMessage(msg []byte) {
	const previewMax = 300
	safe := sensitiveField.ReplaceAll(msg, []byte(`"$1":"<redacted>"`))
	preview := safe
	truncated := ""
	if len(preview) > previewMax {
		preview = preview[:previewMax]
		truncated = fmt.Sprintf("…(+%dB)", len(safe)-previewMax)
	}
	log.Printf("SIGNAL [%dB] %q%s", len(msg), preview, truncated)
}
