package okapi

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestEncodeURIComponent(t *testing.T) {
	cases := map[string]string{
		"abcXYZ019": "abcXYZ019",
		"-_.!~*'()": "-_.!~*'()", // all unreserved, unchanged
		"a b":       "a%20b",     // space is %20, not '+'
		"a+b/c=d&e": "a%2Bb%2Fc%3Dd%26e",
		"й":         "%D0%B9", // UTF-8 multibyte
		`{"k":"v"}`: "%7B%22k%22%3A%22v%22%7D",
	}
	for in, want := range cases {
		if got := encodeURIComponent(in); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildFormBasics(t *testing.T) {
	body, err := BuildForm("vchat.joinConversationByLink", "APPKEY", "SESSION", Params{
		"joinLink":        "tok123",
		"isVideo":         false,
		"protocolVersion": 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	vals, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("output is not valid form encoding: %v", err)
	}
	want := map[string]string{
		"method":          "vchat.joinConversationByLink",
		"format":          "JSON",
		"application_key": "APPKEY",
		"session_key":     "SESSION",
		"joinLink":        "tok123",
		"isVideo":         "false",
		"protocolVersion": "5",
	}
	for k, v := range want {
		if got := vals.Get(k); got != v {
			t.Errorf("field %q = %q, want %q", k, got, v)
		}
	}
}

func TestBuildFormSkipAuthOmitsSession(t *testing.T) {
	// auth.anonymLogin passes an empty sessionKey: session_key must not appear.
	body, err := BuildForm("auth.anonymLogin", "APPKEY", "", Params{
		"session_data": map[string]any{"client_type": "SDK_JS", "version": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	vals, _ := url.ParseQuery(body)
	if vals.Has("session_key") {
		t.Error("session_key present despite empty sessionKey")
	}
	// Nested object must be JSON-encoded.
	var sd map[string]any
	if err := json.Unmarshal([]byte(vals.Get("session_data")), &sd); err != nil {
		t.Fatalf("session_data not JSON: %v (%q)", err, vals.Get("session_data"))
	}
	if sd["client_type"] != "SDK_JS" {
		t.Errorf("session_data.client_type = %v", sd["client_type"])
	}
}

func TestBuildFormCallerApplicationKeyWins(t *testing.T) {
	body, _ := BuildForm("m", "DEFAULT", "", Params{"application_key": "OVERRIDE"})
	vals, _ := url.ParseQuery(body)
	if got := vals.Get("application_key"); got != "OVERRIDE" {
		t.Errorf("application_key = %q, want OVERRIDE", got)
	}
}

func TestParseAPIError(t *testing.T) {
	errBody := []byte(`{"error_code":103,"error_msg":"PARAM_SESSION_KEY : Session key is required","error_data":null}`)
	if err := parseAPIError(errBody); err == nil {
		t.Error("expected error for error envelope")
	} else if ae, ok := err.(*apiError); !ok || ae.Code != 103 {
		t.Errorf("got %v, want apiError code 103", err)
	}
	okBody := []byte(`{"session_key":"abc","serverTime":123}`)
	if err := parseAPIError(okBody); err != nil {
		t.Errorf("unexpected error for ok body: %v", err)
	}
	if err := parseAPIError([]byte(`not json`)); err != nil {
		t.Errorf("non-JSON should yield no apiError, got %v", err)
	}
	// error_code as a string must still surface as an error.
	if err := parseAPIError([]byte(`{"error_code":"42","error_msg":"x"}`)); err == nil {
		t.Error("expected error for string error_code")
	} else if ae, ok := err.(*apiError); !ok || ae.Code != 42 {
		t.Errorf("string code: got %v", err)
	}
	// A zero string code is not an error.
	if err := parseAPIError([]byte(`{"error_code":"0"}`)); err != nil {
		t.Errorf(`"0" code should be no error, got %v`, err)
	}
}

func TestDescriptorParse(t *testing.T) {
	// Structural sample (synthetic values) matching a real join response shape.
	sample := []byte(`{
		"client_type":"VK","device_idx":2,"id":"conv-id","p2p_forbidden":true,
		"token":"ws-token",
		"endpoint":"wss://videowebrtc.okcdn.ru/ws2",
		"wt_endpoint":"https://videowebrtc.okcdn.ru:23456/wt",
		"ws_ip_addresses":["155.212.204.173","155.212.205.229"],
		"stun_server":{"urls":["stun:90.156.236.124:19302"]},
		"turn_server":{"urls":["turn:90.156.236.124:19302","turn:90.156.236.85:19302"],"username":"u","credential":"c"}
	}`)
	var d Descriptor
	if err := json.Unmarshal(sample, &d); err != nil {
		t.Fatal(err)
	}
	if d.Endpoint == "" || d.WtEndpoint == "" {
		t.Error("endpoints not parsed")
	}
	if len(d.TurnServer.URLs) != 2 || d.TurnServer.Credential != "c" {
		t.Errorf("turn server parse: %+v", d.TurnServer)
	}
	if len(d.StunServer.URLs) != 1 {
		t.Errorf("stun server parse: %+v", d.StunServer)
	}
}

func TestBuildFormStringifyError(t *testing.T) {
	if _, err := BuildForm("m", "app", "", Params{"bad": make(chan int)}); err == nil {
		t.Error("an unmarshalable param should make BuildForm error")
	}
}
