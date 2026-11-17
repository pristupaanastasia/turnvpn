package wttransport

import (
	"net/url"
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/wtframe"
)

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse result URL: %v", err)
	}
	return u.Query()
}

func TestBuildSignalingURLWebTransport(t *testing.T) {
	got, err := BuildSignalingURL("https://videowebrtc.okcdn.ru:23456/wt", SignalingParams{
		UserID:          42,
		EntityType:      "GROUP",
		DeviceIdx:       2,
		ConversationID:  "conv-1",
		Token:           "tok",
		Platform:        "web",
		AppVersion:      "0.0.0",
		ProtocolVersion: 5,
		Device:          "dev",
		Capabilities:    "6F7F",
		ClientType:      "VK",
		UserAgent:       "UA/1.0",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, got)
	want := map[string]string{
		"userId": "42", "entityType": "GROUP", "deviceIdx": "2",
		"conversationId": "conv-1", "token": "tok",
		"platform": "web", "appVersion": "0.0.0", "version": "5",
		"device": "dev", "capabilities": "6F7F", "clientType": "VK",
		"compression": "deflate-raw", "ua": "UA/1.0",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("param %q = %q, want %q", k, got, v)
		}
	}
}

func TestBuildSignalingURLWebSocketOmitsWTParams(t *testing.T) {
	got, err := BuildSignalingURL("wss://videowebrtc.okcdn.ru/ws2", SignalingParams{
		UserID: 1, DeviceIdx: 0, Token: "t", UserAgent: "UA",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, got)
	if q.Has("compression") {
		t.Error("compression set on non-WebTransport URL")
	}
	if q.Has("ua") {
		t.Error("ua set on non-WebTransport URL")
	}
	if q.Get("version") != "5" { // default applied
		t.Errorf("version = %q, want default 5", q.Get("version"))
	}
}

func TestBuildSignalingURLPreservesBaseQuery(t *testing.T) {
	got, err := BuildSignalingURL("https://host/wt?existing=1", SignalingParams{UserID: 1}, true)
	if err != nil {
		t.Fatal(err)
	}
	if q := mustQuery(t, got); q.Get("existing") != "1" {
		t.Errorf("base query param dropped: %v", q)
	}
}

func TestBuildSignalingURLRejectsRelative(t *testing.T) {
	if _, err := BuildSignalingURL("/just/a/path", SignalingParams{}, true); err == nil {
		t.Error("expected error for non-absolute base")
	}
}

func TestCompressionFromURL(t *testing.T) {
	cases := map[string]wtframe.Compression{
		"https://h/wt?compression=deflate-raw": wtframe.DeflateRaw,
		"https://h/wt?compression=gzip":        wtframe.GZIP,
		"https://h/wt?compression=deflate":     wtframe.Deflate,
		"https://h/wt":                         wtframe.None,
		"https://h/wt?compression=lz4":         wtframe.None,
	}
	for in, want := range cases {
		if got := CompressionFromURL(in); got != want {
			t.Errorf("CompressionFromURL(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestWebTransportCompressionConstant documents the invariant that the WT leg uses
// deflate-raw, so wtframe/wtsignal must be configured to match.
func TestWebTransportCompressionConstant(t *testing.T) {
	if WebTransportCompression != wtframe.DeflateRaw {
		t.Fatalf("WebTransportCompression = %d, want DeflateRaw", WebTransportCompression)
	}
	got, _ := BuildSignalingURL("https://h/wt", SignalingParams{UserID: 1}, true)
	if CompressionFromURL(got) != WebTransportCompression {
		t.Error("URL compression param does not match WebTransportCompression")
	}
}
