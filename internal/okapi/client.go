package okapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// DefaultAPIServer is the OK API base the web client falls back to; the live
// value can also arrive as api_server in an auth.anonymLogin response.
const DefaultAPIServer = "https://calls.okcdn.ru"

// maxResponseBytes bounds an API response body (these are small JSON payloads).
const maxResponseBytes = 8 << 20 // 8 MiB

// Protocol constants observed from the web client. These are not secrets; they
// identify the client/protocol and may change with client releases.
const (
	// ProtocolVersion is the vchat protocol version the client advertises.
	ProtocolVersion = 5
	// WebClientType is the client_type the web SDK reports in auth.anonymLogin.
	WebClientType = "SDK_JS"
	// DefaultCapabilities is the capability flag string the web client sent
	// (ap.getFlags()); treat it as an opaque, client-version-dependent token.
	DefaultCapabilities = "6F7F"
)

// Client issues OK API calls for one call session. It is safe for sequential use;
// AnonymLogin populates SessionKey, which later calls reuse.
type Client struct {
	HTTP       *http.Client
	APIServer  string // base URL, no trailing slash; defaults to DefaultAPIServer
	AppKey     string // application_key (public web-client key)
	SessionKey string // set by AnonymLogin; sent on authenticated calls
}

// apiError is the OK API's error envelope.
type apiError struct {
	Code int    `json:"error_code"`
	Msg  string `json:"error_msg"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("okapi: server error %d: %s", e.Code, e.Msg)
}

// call POSTs one method to {APIServer}/fb.do and returns the raw JSON body. When
// skipAuth is true the session_key is omitted (used for auth.anonymLogin). It
// returns an *apiError when the response carries error_code.
func (c *Client) call(ctx context.Context, method string, p Params, skipAuth bool) ([]byte, error) {
	base := c.APIServer
	if base == "" {
		base = DefaultAPIServer
	}
	session := c.SessionKey
	if skipAuth {
		session = ""
	}
	body, err := BuildForm(method, c.AppKey, session, p)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/fb.do", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("okapi: %s: %w", method, err)
	}
	defer resp.Body.Close()
	// Bound the response: these are small JSON bodies; cap as hardening.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("okapi: %s: read body: %w", method, err)
	}
	if apiErr := parseAPIError(data); apiErr != nil {
		return data, apiErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, fmt.Errorf("okapi: %s: http %d", method, resp.StatusCode)
	}
	return data, nil
}

// parseAPIError returns an *apiError if data is an OK error envelope, else nil.
// It tolerates error_code encoded as either a JSON number or string so a stray
// string code is still surfaced as an error rather than mistaken for success.
func parseAPIError(data []byte) error {
	var probe struct {
		Code json.RawMessage `json:"error_code"`
		Msg  string          `json:"error_msg"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil // not a JSON object; let the caller decode
	}
	raw := strings.TrimSpace(string(probe.Code))
	if raw == "" || raw == "null" || raw == "0" || raw == `"0"` || raw == `""` {
		return nil
	}
	code, _ := strconv.Atoi(strings.Trim(raw, `"`)) // 0 if non-numeric; message still carries it
	return &apiError{Code: code, Msg: probe.Msg}
}

// AnonymLoginResult is the subset of auth.anonymLogin we consume.
type AnonymLoginResult struct {
	SessionKey       string `json:"session_key"`
	SessionSecretKey string `json:"session_secret_key"`
	UID              string `json:"uid"`
	APIServer        string `json:"api_server"`
}

// AnonymLogin performs auth.anonymLogin with the per-call guest token and returns
// the OK session. On success it stores SessionKey (and APIServer, if returned) on
// the client for subsequent calls. callToken is the VK-issued guest call token;
// deviceID is a stable per-device UUID; clientVersion identifies the client build.
func (c *Client) AnonymLogin(ctx context.Context, callToken, deviceID, clientVersion string) (*AnonymLoginResult, error) {
	data, err := c.call(ctx, "auth.anonymLogin", Params{
		"session_data": map[string]any{
			"device_id":      deviceID,
			"client_version": clientVersion,
			"client_type":    WebClientType,
			"auth_token":     callToken,
			"version":        3,
		},
	}, true)
	if err != nil {
		return nil, err
	}
	var res AnonymLoginResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("okapi: anonymLogin: decode: %w", err)
	}
	if res.SessionKey == "" {
		return nil, fmt.Errorf("okapi: anonymLogin: empty session_key")
	}
	c.SessionKey = res.SessionKey
	// Only adopt a returned api_server with an explicit http(s) scheme; a
	// scheme-less value would break request URL construction.
	if s := strings.TrimRight(res.APIServer, "/"); strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		c.APIServer = s
	}
	return &res, nil
}

// ICEServer mirrors one stun_server/turn_server entry of the join descriptor.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Descriptor is the vchat.joinConversationByLink response: everything needed to
// reach the SFU — the signaling endpoint(s), the per-session token, and ICE
// servers (whose TURN credentials are short-lived and reissued on each join).
type Descriptor struct {
	ClientType    string    `json:"client_type"`
	DeviceIdx     int       `json:"device_idx"`
	ID            string    `json:"id"`
	P2PForbidden  bool      `json:"p2p_forbidden"`
	Token         string    `json:"token"`
	Endpoint      string    `json:"endpoint"`    // wss:// signaling WebSocket (fallback)
	WtEndpoint    string    `json:"wt_endpoint"` // https:// WebTransport endpoint (preferred)
	WsIPAddresses []string  `json:"ws_ip_addresses"`
	WtIPAddresses []string  `json:"wt_ip_addresses"`
	StunServer    ICEServer `json:"stun_server"`
	TurnServer    ICEServer `json:"turn_server"`
}

// JoinOptions tunes a join request; the zero value is a valid audio-only join.
type JoinOptions struct {
	IsVideo      bool   // request a video session
	AnonymToken  string // VK guest token, when joining as an anonymous guest
	Capabilities string // override the capability flags; empty uses DefaultCapabilities
}

// JoinConversationByLink joins the call identified by joinLink (the token from a
// vk.ru/call/join/<token> URL) and returns the SFU descriptor. AnonymLogin (or an
// otherwise-populated SessionKey) must have run first.
func (c *Client) JoinConversationByLink(ctx context.Context, joinLink string, opts JoinOptions) (*Descriptor, error) {
	if c.SessionKey == "" {
		return nil, fmt.Errorf("okapi: joinConversationByLink: no session (call AnonymLogin first)")
	}
	caps := opts.Capabilities
	if caps == "" {
		caps = DefaultCapabilities
	}
	p := Params{
		"joinLink":        joinLink,
		"isVideo":         opts.IsVideo,
		"protocolVersion": ProtocolVersion,
		"capabilities":    caps,
	}
	if opts.AnonymToken != "" {
		p["anonymToken"] = opts.AnonymToken
	}
	data, err := c.call(ctx, "vchat.joinConversationByLink", p, false)
	if err != nil {
		return nil, err
	}
	var d Descriptor
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("okapi: joinConversationByLink: decode: %w", err)
	}
	if d.Endpoint == "" && d.WtEndpoint == "" {
		return nil, fmt.Errorf("okapi: joinConversationByLink: descriptor has no endpoint")
	}
	return &d, nil
}
