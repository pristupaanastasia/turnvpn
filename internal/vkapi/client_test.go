package vkapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetCallToken(t *testing.T) {
	var gotPath string
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
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
	for k, want := range map[string]string{
		"vk_join_link": "linktok", "name": "probe", "access_token": "ANON", "v": APIVersion,
	} {
		if got := gotForm[k]; len(got) != 1 || got[0] != want {
			t.Errorf("form %q = %v, want %q", k, got, want)
		}
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

func TestRedactEndpoint(t *testing.T) {
	if got := redactEndpoint("https://h/method/x?access_token=SECRET&v=5"); got != "https://h/method/x" {
		t.Errorf("redactEndpoint = %q", got)
	}
	if got := redactEndpoint("https://h/method/x"); got != "https://h/method/x" {
		t.Errorf("redactEndpoint (no query) = %q", got)
	}
}
