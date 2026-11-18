// Package vkapi reproduces the VK-side bootstrap that yields a guest call token
// for VK Звонки, so a headless client can join without a browser. Reversed from
// the web client (chunk webCallsBridge.js); see
// tools/fbdo-capture/signaling-reference.md.
//
// The flow has two steps:
//
//  1. GetAnonymToken: obtain an anonymous VK access token from the VK login
//     service (login.vk.ru, method get_anonym_token) using the public web client
//     id/secret. EXPERIMENTAL — the exact login request shape lives in a VK login
//     module not yet captured; verify against a real get_anonym_token call.
//  2. GetCallToken: call calls.getAnonymousToken(vk_join_link, name) on the VK API
//     with that access token to get the per-call guest token. This token feeds
//     okapi (auth.anonymLogin auth_token and joinConversationByLink anonymToken).
//
// GetCallToken rests on the standard, public VK API request form plus the
// chunk-confirmed parameter names, so it is the grounded, tested part.
package vkapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Public constants from the web client (chunk webCallsBridge.js). Not secrets:
// the client_id/secret identify the public VK web application.
const (
	// WebClientID is the VK web application id (d.u.WEB); also seen as client_id
	// in calls.subscribeToQueue.
	WebClientID = "6287487"
	// AnonClientSecret is the public web-client secret used for get_anonym_token.
	AnonClientSecret = "QbYic1K3lEV5kTGiqlq2"
	// APIVersion is the VK API version the web client sends (observed live;
	// drifts with releases).
	APIVersion = "5.289"
	// AnonTokenType is the token_type passed to get_anonym_token.
	AnonTokenType = "messages"
	// AnonTokenVersion is the version passed to get_anonym_token (from the chunk).
	AnonTokenVersion = "5.226"
)

// Default service hosts.
const (
	DefaultAPIBase   = "https://web.api.vk.ru"
	DefaultLoginBase = "https://login.vk.ru"
)

// maxResponseBytes bounds a response body (VK API replies are small JSON).
const maxResponseBytes = 8 << 20

// Client calls the VK API and login service.
type Client struct {
	HTTP      *http.Client
	APIBase   string // default DefaultAPIBase
	LoginBase string // default DefaultLoginBase
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) loginBase() string {
	if c.LoginBase != "" {
		return strings.TrimRight(c.LoginBase, "/")
	}
	return DefaultLoginBase
}

// vkError is the VK API error envelope.
type vkError struct {
	Code int    `json:"error_code"`
	Msg  string `json:"error_msg"`
}

func (e *vkError) Error() string { return fmt.Sprintf("vkapi: error %d: %s", e.Code, e.Msg) }

// VK error codes we special-case.
const (
	errCodeFlood   = 9  // "Flood control"
	errCodeCaptcha = 14 // "Captcha needed"
)

// CaptchaError means VK demands a captcha (anti-abuse). It is terminal for a
// headless client: the captcha must be solved by a human in a browser (see
// RedirectURI); it is NOT cleared by retrying, and must not be bypassed.
type CaptchaError struct {
	Code        int
	Msg         string
	RedirectURI string
}

func (e *CaptchaError) Error() string {
	return fmt.Sprintf("vkapi: captcha required (error %d: %s); solve it in a browser — it cannot be retried away", e.Code, e.Msg)
}

// FloodError means VK is rate-limiting (flood control). Unlike a captcha it is
// transient, so a polite backoff-and-retry is reasonable.
type FloodError struct {
	Code int
	Msg  string
}

func (e *FloodError) Error() string {
	return fmt.Sprintf("vkapi: flood control (error %d: %s); back off and retry", e.Code, e.Msg)
}

// classifyError maps a VK error envelope to a typed error: captcha, flood, or a
// generic vkError.
func classifyError(code int, msg, redirectURI string, captchaEnabled bool) error {
	if captchaEnabled || code == errCodeCaptcha {
		return &CaptchaError{Code: code, Msg: msg, RedirectURI: redirectURI}
	}
	if code == errCodeFlood || strings.Contains(strings.ToLower(msg), "flood") {
		return &FloodError{Code: code, Msg: msg}
	}
	return &vkError{Code: code, Msg: msg}
}

// postForm POSTs application/x-www-form-urlencoded and returns the body.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("vkapi: POST %s: %w", redactEndpoint(endpoint), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, fmt.Errorf("vkapi: http %d from %s", resp.StatusCode, redactEndpoint(endpoint))
	}
	return data, nil
}

// GetCallToken calls calls.getAnonymousToken and returns the per-call guest token
// used by okapi. accessToken is the anonymous VK access token (from
// GetAnonymToken or captured); joinLink is the token from vk.ru/call/join/<token>;
// name is the guest display name.
func (c *Client) GetCallToken(ctx context.Context, accessToken, joinLink, name string) (string, error) {
	if accessToken == "" || joinLink == "" {
		return "", fmt.Errorf("vkapi: GetCallToken needs accessToken and joinLink")
	}
	// Observed live form: v and client_id go in the query; vk_join_link, name and
	// access_token go in the POST body.
	endpoint := c.apiBase() + "/method/calls.getAnonymousToken" +
		"?v=" + url.QueryEscape(APIVersion) + "&client_id=" + url.QueryEscape(WebClientID)
	form := url.Values{
		"vk_join_link": {joinLink},
		"name":         {name},
		"access_token": {accessToken},
	}
	data, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return "", err
	}
	var env struct {
		Response struct {
			Token string `json:"token"`
		} `json:"response"`
		Error *struct {
			Code           int    `json:"error_code"`
			Msg            string `json:"error_msg"`
			CaptchaEnabled bool   `json:"is_enabled_captcha"`
			RedirectURI    string `json:"redirect_uri"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("vkapi: decode getAnonymousToken: %w", err)
	}
	if env.Error != nil {
		return "", classifyError(env.Error.Code, env.Error.Msg, env.Error.RedirectURI, env.Error.CaptchaEnabled)
	}
	if env.Response.Token == "" {
		return "", fmt.Errorf("vkapi: getAnonymousToken returned no token")
	}
	return env.Response.Token, nil
}

// RetryPolicy controls the polite backoff for transient flood-control errors.
// A MaxAttempts <= 1 means a single attempt (no retry).
type RetryPolicy struct {
	MaxAttempts int           // total attempts including the first
	BaseDelay   time.Duration // first backoff, doubled each attempt
	MaxDelay    time.Duration // cap on one backoff (0 = uncapped)
}

// DefaultRetryPolicy is a conservative, low-footprint policy for flood control.
var DefaultRetryPolicy = RetryPolicy{MaxAttempts: 4, BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}

// GetCallTokenRetry is GetCallToken with a polite backoff that retries ONLY on
// flood control (a transient rate limit). A captcha or any other error returns
// immediately: a captcha must be solved by a human and must never be retried away.
func (c *Client) GetCallTokenRetry(ctx context.Context, accessToken, joinLink, name string, p RetryPolicy) (string, error) {
	attempts := p.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	delay := p.BaseDelay
	for i := 0; i < attempts; i++ {
		tok, err := c.GetCallToken(ctx, accessToken, joinLink, name)
		if err == nil {
			return tok, nil
		}
		var fe *FloodError
		if !errors.As(err, &fe) || i == attempts-1 {
			return "", err // not flood, or no attempts left
		}
		d := delay
		if p.MaxDelay > 0 && d > p.MaxDelay {
			d = p.MaxDelay
		}
		if d > 0 {
			d += time.Duration(rand.Int63n(int64(d)/2 + 1)) // up to +50% jitter
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(d):
		}
		delay *= 2
	}
	return "", errors.New("vkapi: GetCallTokenRetry exhausted attempts")
}

// GetAnonymToken obtains an anonymous VK access token via the login service's
// get_anonym_token method, using the public web client id/secret.
//
// EXPERIMENTAL: the request shape (the login.vk.ru endpoint/act and parameter
// encoding) is taken from the known constants and VK's login conventions, but the
// exact form lives in a VK login module not yet captured. Verify it against a real
// get_anonym_token call (capture one from a fresh, cookie-cleared guest join); if
// it differs, only the endpoint/params here change. Prefer supplying a captured
// access token to GetCallToken until this is confirmed.
func (c *Client) GetAnonymToken(ctx context.Context) (string, error) {
	form := url.Values{
		"act":           {"get_anonym_token"},
		"client_id":     {WebClientID},
		"client_secret": {AnonClientSecret},
		"token_type":    {AnonTokenType},
		"version":       {AnonTokenVersion},
	}
	data, err := c.postForm(ctx, c.loginBase()+"/", form)
	if err != nil {
		return "", err
	}
	var env struct {
		AccessToken string `json:"access_token"`
		Response    struct {
			AccessToken string `json:"access_token"`
		} `json:"response"`
		// error can be an OAuth string ("invalid_client") or a VK object.
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("vkapi: decode get_anonym_token: %w", err)
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return "", anonTokenError(env.Error, env.ErrorDescription)
	}
	tok := env.AccessToken
	if tok == "" {
		tok = env.Response.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("vkapi: get_anonym_token returned no access_token (endpoint form may need verifying)")
	}
	return tok, nil
}

// anonTokenError turns the login service's error field — an OAuth-style string
// ("invalid_client") or a VK error object — into a readable error.
func anonTokenError(raw json.RawMessage, desc string) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if desc != "" {
			return fmt.Errorf("vkapi: get_anonym_token: %s: %s", s, desc)
		}
		return fmt.Errorf("vkapi: get_anonym_token: %s", s)
	}
	var ve vkError
	if json.Unmarshal(raw, &ve) == nil && ve.Code != 0 {
		return &ve
	}
	return fmt.Errorf("vkapi: get_anonym_token error: %s", string(raw))
}

// redactEndpoint strips any query from an endpoint for safe logging.
func redactEndpoint(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}
