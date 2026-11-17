// Command vkjoin drives the VK Звонки join end to end from the reversed stack:
// it authenticates (OK auth.anonymLogin), joins the conversation
// (vchat.joinConversationByLink), builds the signaling URL, dials it over
// WebTransport, and prints the signaling messages that arrive. It is the capture
// tool for the next reverse step — understanding the signaling message schema —
// the same capture→understand method used for the REST layer.
//
// It needs a live guest call token (the VK calls.getAnonymousToken flow is not
// yet reproduced, so pass one captured from a browser session via -token). Tokens
// are never printed.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/graceful"
	"github.com/pristupaanastasia/turnvpn/internal/okapi"
	"github.com/pristupaanastasia/turnvpn/internal/wttransport"
)

func main() {
	link := flag.String("link", "", "join link token from vk.ru/call/join/<token> (required)")
	token := flag.String("token", "", "VK guest call token (auth_token/anonymToken); required")
	appKey := flag.String("app-key", "CGMMEJLGDIHBABABA", "OK application_key (public web-client key)")
	deviceID := flag.String("device-id", "turnvpn-vkjoin", "stable device id for anonymLogin")
	clientVer := flag.String("client-version", "0.0.0", "client_version for anonymLogin")
	entityType := flag.String("entity-type", "", "signaling entityType (from conversation params; may be required)")
	video := flag.Bool("video", false, "request a video session")
	dur := flag.Duration("duration", 30*time.Second, "how long to listen for signaling messages")
	flag.Parse()

	if *link == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "usage: vkjoin -link <token> -token <guest-call-token> [flags]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	r := graceful.New()
	defer r.Stop()

	if err := run(r.Context(), config{
		link: *link, token: *token, appKey: *appKey, deviceID: *deviceID,
		clientVersion: *clientVer, entityType: *entityType, video: *video, duration: *dur,
	}); err != nil {
		log.Fatalf("vkjoin: %v", err)
	}
}

type config struct {
	link, token, appKey, deviceID, clientVersion, entityType string
	video                                                    bool
	duration                                                 time.Duration
}

func run(ctx context.Context, cfg config) error {
	cl := &okapi.Client{AppKey: cfg.appKey}

	login, err := cl.AnonymLogin(ctx, cfg.token, cfg.deviceID, cfg.clientVersion)
	if err != nil {
		return fmt.Errorf("anonymLogin: %w", err)
	}
	log.Printf("anonymLogin ok (uid=%s, api=%s)", login.UID, cl.APIServer)

	desc, err := cl.JoinConversationByLink(ctx, cfg.link, okapi.JoinOptions{
		IsVideo:     cfg.video,
		AnonymToken: cfg.token,
	})
	if err != nil {
		return fmt.Errorf("joinConversationByLink: %w", err)
	}
	log.Printf("joined: endpoint=%s wt=%s turn=%v", desc.Endpoint, desc.WtEndpoint, desc.TurnServer.URLs)

	params, useWT, err := buildParams(login, desc, cfg)
	if err != nil {
		return err
	}
	base := desc.WtEndpoint
	if !useWT {
		base = desc.Endpoint
	}
	sigURL, err := wttransport.BuildSignalingURL(base, params, useWT)
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
	log.Printf("signaling connected (compression=%s); listening %s", desc.WtEndpoint, cfg.duration)

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

// buildParams maps the login + descriptor onto the signaling URL parameters, and
// reports whether to use the WebTransport endpoint (preferred when present).
func buildParams(login *okapi.AnonymLoginResult, desc *okapi.Descriptor, cfg config) (wttransport.SignalingParams, bool, error) {
	var uid int64
	if login.UID != "" {
		v, err := strconv.ParseInt(login.UID, 10, 64)
		if err != nil {
			return wttransport.SignalingParams{}, false, fmt.Errorf("parse uid %q: %w", login.UID, err)
		}
		uid = v
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

// logMessage prints a captured signaling message for analysis. Signaling payloads
// may carry tokens/SDP crypto, so this prints length and a bounded preview only.
func logMessage(msg []byte) {
	const previewMax = 300
	preview := msg
	truncated := ""
	if len(preview) > previewMax {
		preview = preview[:previewMax]
		truncated = fmt.Sprintf("…(+%d)", len(msg)-previewMax)
	}
	log.Printf("SIGNAL [%dB] %s%s", len(msg), preview, truncated)
}
