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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Public constants from the web client (chunk webCallsBridge.js). Not secrets:
// the client_id/secret identify the public VK web application.
const (
	// WebClientID is the VK web application id (d.u.WEB); also seen as client_id
	// in calls.subscribeToQueue.
	WebClientID = "6287487"
	// AnonClientSecret is the public web-client secret used for get_anonym_token.
	AnonClientSecret = "QbYic1K3lEV5kTGiqlq2"
	// APIVersion is the VK API version the web client sends.
	APIVersion = "5.226"
	// AnonTokenType is the token_type passed to get_anonym_token.
	AnonTokenType = "messages"
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

// postForm POSTs application/x-www-form-urlencoded and returns the body.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
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
	form := url.Values{
		"vk_join_link": {joinLink},
		"name":         {name},
		"access_token": {accessToken},
		"v":            {APIVersion},
	}
	data, err := c.postForm(ctx, c.apiBase()+"/method/calls.getAnonymousToken", form)
	if err != nil {
		return "", err
	}
	var env struct {
		Response struct {
			Token string `json:"token"`
		} `json:"response"`
		Error *vkError `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("vkapi: decode getAnonymousToken: %w", err)
	}
	if env.Error != nil {
		return "", env.Error
	}
	if env.Response.Token == "" {
		return "", fmt.Errorf("vkapi: getAnonymousToken returned no token")
	}
	return env.Response.Token, nil
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
		"version":       {APIVersion},
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
		Error     *vkError `json:"error"`
		ErrorText string   `json:"error_type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("vkapi: decode get_anonym_token: %w", err)
	}
	if env.Error != nil {
		return "", env.Error
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

// redactEndpoint strips any query from an endpoint for safe logging.
func redactEndpoint(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}
