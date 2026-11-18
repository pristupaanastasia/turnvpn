package vkapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGetCallToken(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{"token":"CALLTOK"}}`))
	}))
	defer srv.Close()

	c := &Client{APIBase: srv.URL}
	tok, err := c.GetCallToken(context.Background(), "ANON", "linktok", "probe")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "CALLTOK" {
		t.Errorf("token = %q, want CALLTOK", tok)
	}
	if gotPath != "/method/calls.getAnonymousToken" {
		t.Errorf("path = %q", gotPath)
	}
	// v and client_id are in the query (observed live form).
	if gotQuery.Get("v") != APIVersion || gotQuery.Get("client_id") != WebClientID {
		t.Errorf("query = %v, want v=%s client_id=%s", gotQuery, APIVersion, WebClientID)
	}
	// vk_join_link, name, access_token are in the body; v is NOT.
	for k, want := range map[string]string{
		"vk_join_link": "linktok", "name": "probe", "access_token": "ANON",
	} {
		if got := gotForm[k]; len(got) != 1 || got[0] != want {
			t.Errorf("form %q = %v, want %q", k, got, want)
		}
	}
	if gotForm["v"] != nil {
		t.Errorf("v should be in the query, not the body")
	}
}

func TestGetCallTokenCaptcha(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"error_code":14,"error_msg":"Captcha need","is_enabled_captcha":true}}`))
	}))
	defer srv.Close()
	c := &Client{APIBase: srv.URL}
	_, err := c.GetCallToken(context.Background(), "A", "L", "n")
	if err == nil {
		t.Fatal("expected captcha error")
	}
	if !strings.Contains(err.Error(), "captcha") {
		t.Errorf("error = %q, want it to mention captcha", err)
	}
}

func TestGetCallTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"error_code":15,"error_msg":"Access denied"}}`))
	}))
	defer srv.Close()
	c := &Client{APIBase: srv.URL}
	if _, err := c.GetCallToken(context.Background(), "A", "L", "n"); err == nil {
		t.Error("expected error for error envelope")
	} else if ve, ok := err.(*vkError); !ok || ve.Code != 15 {
		t.Errorf("got %v, want vkError 15", err)
	}
}

func TestGetCallTokenValidation(t *testing.T) {
	c := &Client{}
	if _, err := c.GetCallToken(context.Background(), "", "L", "n"); err == nil {
		t.Error("expected error for empty access token")
	}
	if _, err := c.GetCallToken(context.Background(), "A", "", "n"); err == nil {
		t.Error("expected error for empty join link")
	}
}

func TestGetCallTokenNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response":{}}`))
	}))
	defer srv.Close()
	c := &Client{APIBase: srv.URL}
	if _, err := c.GetCallToken(context.Background(), "A", "L", "n"); err == nil {
		t.Error("expected error when response has no token")
	}
}

func TestGetAnonymToken(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"ANON"}`,
		`{"response":{"access_token":"ANON"}}`,
	} {
		var gotForm map[string][]string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			gotForm = r.PostForm
			w.Write([]byte(body))
		}))
		c := &Client{LoginBase: srv.URL}
		tok, err := c.GetAnonymToken(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
		if tok != "ANON" {
			t.Errorf("body %s: token = %q", body, tok)
		}
		if gotForm["act"][0] != "get_anonym_token" || gotForm["client_id"][0] != WebClientID {
			t.Errorf("form = %v", gotForm)
		}
	}
}

func TestGetAnonymTokenOAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"invalid_client","error_description":"bad secret"}`))
	}))
	defer srv.Close()
	c := &Client{LoginBase: srv.URL}
	_, err := c.GetAnonymToken(context.Background())
	if err == nil {
		t.Fatal("expected OAuth error")
	}
	if got := err.Error(); !strings.Contains(got, "invalid_client") || !strings.Contains(got, "bad secret") {
		t.Errorf("error = %q, want it to mention invalid_client and the description", got)
	}
}

func TestRedactEndpoint(t *testing.T) {
	if got := redactEndpoint("https://h/method/x?access_token=SECRET&v=5"); got != "https://h/method/x" {
		t.Errorf("redactEndpoint = %q", got)
	}
	if got := redactEndpoint("https://h/method/x"); got != "https://h/method/x" {
		t.Errorf("redactEndpoint (no query) = %q", got)
	}
}
