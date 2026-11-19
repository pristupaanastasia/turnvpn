package carrier

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTokenCredsFieldOrder guards the TURN-creds field mapping: fetchTurnCreds yields
// (addr, user, pass) but the Creds contract is (user, pass, addr). A swap (the live
// bug) made the TURN address the credential and failed with "missing port". Distinct
// values here make any swap a test failure.
func TestTokenCredsFieldOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "auth.anonymLogin"):
			_, _ = io.WriteString(w, `{"session_key":"SK"}`)
		case strings.Contains(string(body), "vchat.joinConversationByLink"):
			_, _ = io.WriteString(w, `{"endpoint":"wss://sfu.example/x","turn_server":{`+
				`"urls":["turn:1.2.3.4:3478?transport=udp"],`+
				`"username":"USERVAL","credential":"PASSVAL"}}`)
		default:
			t.Errorf("unexpected request body: %s", body)
		}
	}))
	defer srv.Close()

	creds := tokenCreds("appkey", "calllink", "calltoken", srv.URL)
	user, pass, addr, err := creds(context.Background())
	if err != nil {
		t.Fatalf("creds: %v", err)
	}
	if user != "USERVAL" {
		t.Errorf("user = %q, want USERVAL (fields swapped?)", user)
	}
	if pass != "PASSVAL" {
		t.Errorf("pass = %q, want PASSVAL", pass)
	}
	if addr != "1.2.3.4:3478" {
		t.Errorf("addr = %q, want 1.2.3.4:3478 (should be the TURN host:port)", addr)
	}
}
