// Command vkjoin drives the VK Звонки join end to end from the reversed stack:
// it authenticates (OK auth.anonymLogin), joins the conversation
// (vchat.joinConversationByLink), builds the signaling URL, dials it over
// WebTransport, and prints the signaling messages that arrive. It is the capture
// tool for the next reverse step — understanding the signaling message schema —
// the same capture→understand method used for the REST layer.
//
// It needs a live guest call token (the VK calls.getAnonymousToken flow is not
// yet reproduced, so pass one captured from a browser session via -token or the
// VKJOIN_TOKEN env var). The CLI never prints its own tokens; captured signaling
// message content is logged quoted and length-bounded but is not itself redacted.
package main

import (
	"context"
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
	"github.com/pristupaanastasia/turnvpn/internal/wttransport"
)

// httpTimeout bounds each OK API request so a stalled server cannot hang the run.
const httpTimeout = 30 * time.Second

func main() {
	link := flag.String("link", "", "join link token from vk.ru/call/join/<token> (required)")
	token := flag.String("token", "", "VK guest call token; takes precedence, else -access-token, else -auto-token")
	accessToken := flag.String("access-token", "", "VK anonymous access token; with -link derives the call token (or env VKJOIN_ACCESS_TOKEN)")
	autoToken := flag.Bool("auto-token", false, "obtain the anonymous access token automatically (experimental get_anonym_token)")
	name := flag.String("name", "probe", "guest display name")
	appKey := flag.String("app-key", "CGMMEJLGDIHBABABA", "OK application_key (public web-client key)")
	deviceID := flag.String("device-id", "turnvpn-vkjoin", "stable device id for anonymLogin")
	clientVer := flag.String("client-version", "0.0.0", "client_version for anonymLogin")
	entityType := flag.String("entity-type", "", "signaling entityType (from conversation params; may be required)")
	video := flag.Bool("video", false, "request a video session")
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
	if *link == "" || (tok == "" && accTok == "" && !*autoToken) {
		fmt.Fprintln(os.Stderr, "usage: vkjoin -link <token> [-token <call-token> | -access-token <vk-token> | -auto-token] [flags]")
		fmt.Fprintln(os.Stderr, "       tokens may also come from VKJOIN_TOKEN / VKJOIN_ACCESS_TOKEN env vars")
		flag.PrintDefaults()
		os.Exit(2)
	}

	r := graceful.New()
	defer r.Stop()

	if err := run(r.Context(), config{
		link: *link, token: tok, accessToken: accTok, autoToken: *autoToken, name: *name,
		appKey: *appKey, deviceID: *deviceID,
		clientVersion: *clientVer, entityType: *entityType, video: *video, duration: *dur,
	}); err != nil {
		log.Fatalf("vkjoin: %v", err)
	}
}

type config struct {
	link, token, accessToken, name              string
	appKey, deviceID, clientVersion, entityType string
	autoToken, video                            bool
	duration                                    time.Duration
}

func run(ctx context.Context, cfg config) error {
	callToken, err := resolveCallToken(ctx, cfg)
	if err != nil {
		return err
	}

	cl := &okapi.Client{AppKey: cfg.appKey, HTTP: &http.Client{Timeout: httpTimeout}}

	login, err := cl.AnonymLogin(ctx, callToken, cfg.deviceID, cfg.clientVersion)
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
	log.Printf("signaling connected (compression=%d); listening %s", wttransport.CompressionFromURL(sigURL), cfg.duration)

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

// resolveCallToken returns the per-call guest token: the explicit -token if set,
// otherwise derived from a VK anonymous access token (explicit, env, or, with
// -auto-token, fetched via the experimental get_anonym_token).
func resolveCallToken(ctx context.Context, cfg config) (string, error) {
	if cfg.token != "" {
		return cfg.token, nil
	}
	vk := &vkapi.Client{HTTP: &http.Client{Timeout: httpTimeout}}
	access := cfg.accessToken
	if access == "" {
		if !cfg.autoToken {
			return "", fmt.Errorf("no call token: pass -token, -access-token, or -auto-token")
		}
		a, err := vk.GetAnonymToken(ctx)
		if err != nil {
			return "", fmt.Errorf("get anonymous access token: %w", err)
		}
		access = a
		log.Printf("obtained anonymous access token (experimental get_anonym_token)")
	}
	// Retry politely on transient flood control; a captcha returns immediately
	// (it must be solved by a human in a browser, never retried away).
	tok, err := vk.GetCallTokenRetry(ctx, access, cfg.link, cfg.name, vkapi.DefaultRetryPolicy)
	if err != nil {
		return "", fmt.Errorf("get call token: %w", err)
	}
	log.Printf("derived call token from access token")
	return tok, nil
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
