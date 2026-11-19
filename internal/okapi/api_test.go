package okapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), APIServer: srv.URL, AppKey: "APP"}
}

func TestAnonymLogin(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "auth.anonymLogin") {
			t.Errorf("unexpected body: %s", body)
		}
		_, _ = io.WriteString(w, `{"session_key":"SK","api_server":"https://other.example"}`)
	})
	res, err := c.AnonymLogin(context.Background(), "device-1")
	if err != nil {
		t.Fatalf("AnonymLogin: %v", err)
	}
	if res.SessionKey != "SK" {
		t.Errorf("session_key = %q", res.SessionKey)
	}
	if c.SessionKey != "SK" {
		t.Errorf("client SessionKey not stored: %q", c.SessionKey)
	}
}

func TestAnonymLoginServerError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"error_code":9,"error_msg":"denied"}`)
	})
	if _, err := c.AnonymLogin(context.Background(), "d"); err == nil {
		t.Fatal("expected server error")
	}
}

func TestJoinConversationByLink(t *testing.T) {
	// No session yet → error before any request.
	c := &Client{AppKey: "APP"}
	if _, err := c.JoinConversationByLink(context.Background(), "link", JoinOptions{}); err == nil {
		t.Fatal("expected error without a session")
	}

	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"endpoint":"wss://sfu.example/x"}`)
	})
	c.SessionKey = "SK"
	d, err := c.JoinConversationByLink(context.Background(), "link", JoinOptions{AnonymToken: "tok"})
	if err != nil {
		t.Fatalf("JoinConversationByLink: %v", err)
	}
	if d.Endpoint != "wss://sfu.example/x" {
		t.Errorf("endpoint = %q", d.Endpoint)
	}

	// A descriptor with no endpoint is rejected.
	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	c2.SessionKey = "SK"
	if _, err := c2.JoinConversationByLink(context.Background(), "link", JoinOptions{}); err == nil {
		t.Error("expected error for descriptor without endpoint")
	}
}

func TestCallHTTPError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "oops")
	})
	if _, err := c.AnonymLogin(contextBg(), "d"); err == nil {
		t.Error("HTTP 500 should surface an error")
	}
}

func contextBg() context.Context { return context.Background() }
