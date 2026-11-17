// Package wttransport builds the VK Звонки signaling socket URL and (later) dials
// it over WebTransport. The URL construction mirrors the web client engine
// (chunk 60828.js: _buildSignalingEndpoint + _buildUrl); see
// tools/fbdo-capture/signaling-reference.md.
//
// The join descriptor (package okapi) yields a base endpoint and a per-session
// token; the client then appends identity and connection query parameters before
// connecting. For a WebTransport connection the client also forces
// compression=deflate-raw, which is why wtframe/wtsignal must use DeflateRaw on
// that leg.
package wttransport

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/pristupaanastasia/turnvpn/internal/wtframe"
)

// WebTransportCompression is the compression the client hardcodes for the
// WebTransport leg (_buildUrl sets ?compression=deflate-raw when WebTransport).
const WebTransportCompression = wtframe.DeflateRaw

// SignalingParams carries everything the client puts on the signaling URL. The
// identity fields come from the join descriptor; the client fields identify the
// build and protocol.
type SignalingParams struct {
	// Identity (from _buildSignalingEndpoint).
	UserID         int64
	EntityType     string // the "entityType" param (conversation entity type)
	DeviceIdx      int
	ConversationID string
	Token          string // per-session signaling token from the descriptor

	// Client/connection (from _buildUrl).
	Platform        string
	AppVersion      string
	ProtocolVersion int // "version" param; defaults to okapi.ProtocolVersion when 0
	Device          string
	Capabilities    string
	ClientType      string
	UserAgent       string // sent as "ua" on the WebTransport leg
}

// BuildSignalingURL returns the full signaling URL to connect to: base with the
// identity and connection query parameters appended, exactly as the web client
// assembles them. When webTransport is true it also sets compression=deflate-raw
// and ua, matching the client. Existing query parameters on base are preserved.
func BuildSignalingURL(base string, p SignalingParams, webTransport bool) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("wttransport: parse base: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("wttransport: base %q is not an absolute URL", base)
	}
	q := u.Query()

	// _buildSignalingEndpoint
	q.Set("userId", strconv.FormatInt(p.UserID, 10))
	if p.EntityType != "" {
		q.Set("entityType", p.EntityType)
	}
	q.Set("deviceIdx", strconv.Itoa(p.DeviceIdx))
	if p.ConversationID != "" {
		q.Set("conversationId", p.ConversationID)
	}
	if p.Token != "" {
		q.Set("token", p.Token)
	}

	// _buildUrl
	if p.Platform != "" {
		q.Set("platform", p.Platform)
	}
	if p.AppVersion != "" {
		q.Set("appVersion", p.AppVersion)
	}
	ver := p.ProtocolVersion
	if ver == 0 {
		ver = 5 // okapi.ProtocolVersion; duplicated to avoid an import cycle risk
	}
	q.Set("version", strconv.Itoa(ver))
	if p.Device != "" {
		q.Set("device", p.Device)
	}
	if p.Capabilities != "" {
		q.Set("capabilities", p.Capabilities)
	}
	if p.ClientType != "" {
		q.Set("clientType", p.ClientType)
	}
	if webTransport {
		q.Set("compression", "deflate-raw")
		if p.UserAgent != "" {
			q.Set("ua", p.UserAgent)
		}
	}

	u.RawQuery = q.Encode()
	return u.String(), nil
}

// CompressionFromURL reads the ?compression= parameter of a signaling URL and maps
// it to a wtframe.Compression, mirroring the client's getCompressionTypeFromUrl
// (unknown or absent means None).
func CompressionFromURL(signalingURL string) wtframe.Compression {
	u, err := url.Parse(signalingURL)
	if err != nil {
		return wtframe.None
	}
	return wtframe.ParseCompression(u.Query().Get("compression"))
}
