package vkapi

import (
	"context"
	"strings"
	"testing"
)

func TestClientDefaults(t *testing.T) {
	c := &Client{}
	if c.apiBase() != DefaultAPIBase {
		t.Errorf("apiBase = %q", c.apiBase())
	}
	if c.loginBase() != DefaultLoginBase {
		t.Errorf("loginBase = %q", c.loginBase())
	}
	if c.httpClient() == nil {
		t.Error("httpClient must never be nil")
	}
	// Overrides are trimmed of a trailing slash.
	c = &Client{APIBase: "https://a.example/", LoginBase: "https://l.example/"}
	if c.apiBase() != "https://a.example" || c.loginBase() != "https://l.example" {
		t.Errorf("override trim failed: %q %q", c.apiBase(), c.loginBase())
	}
}

func TestClassifyError(t *testing.T) {
	// captchaEnabled → CaptchaError regardless of code.
	if _, ok := classifyError(1, "x", "https://redir", true).(*CaptchaError); !ok {
		t.Error("captchaEnabled should yield CaptchaError")
	}
	// captcha code.
	if _, ok := classifyError(errCodeCaptcha, "need captcha", "", false).(*CaptchaError); !ok {
		t.Error("errCodeCaptcha should yield CaptchaError")
	}
	// flood code and flood message.
	if _, ok := classifyError(errCodeFlood, "x", "", false).(*FloodError); !ok {
		t.Error("errCodeFlood should yield FloodError")
	}
	if _, ok := classifyError(123, "Flood control", "", false).(*FloodError); !ok {
		t.Error("flood message should yield FloodError")
	}
	// generic.
	if _, ok := classifyError(500, "boom", "", false).(*vkError); !ok {
		t.Error("other codes should yield a generic vkError")
	}
}

func TestErrorMessages(t *testing.T) {
	for _, e := range []error{
		&vkError{Code: 1, Msg: "a"},
		&CaptchaError{Code: 14, Msg: "b"},
		&FloodError{Code: 9, Msg: "c"},
	} {
		if s := e.Error(); !strings.HasPrefix(s, "vkapi:") || s == "" {
			t.Errorf("bad error string: %q", s)
		}
	}
}

func TestPostFormConnError(t *testing.T) {
	c := &Client{}
	// Port 0 is not connectable → postForm surfaces a transport error.
	if _, err := c.postForm(contextTODO(), "http://127.0.0.1:0/x", nil); err == nil {
		t.Error("postForm to an unreachable endpoint should error")
	}
}

func TestAnonTokenError(t *testing.T) {
	if err := anonTokenError([]byte(`{"error":"bad","error_description":"nope"}`), "d"); err == nil {
		t.Error("error envelope should yield an error")
	}
}

func contextTODO() context.Context { return context.Background() }
