package wggw

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"10.0.0.1", "192.168.1.1", "172.16.0.1", // private
		"169.254.169.254",      // cloud metadata (link-local)
		"224.0.0.1", "ff02::1", // multicast
		"0.0.0.0", // unspecified
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34"}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("nil IP should be blocked")
	}
}

func TestResolveSafe(t *testing.T) {
	if got, err := resolveSafe("1.1.1.1:443"); err != nil || got != "1.1.1.1:443" {
		t.Errorf("resolveSafe(public) = %q, %v", got, err)
	}
	if _, err := resolveSafe("127.0.0.1:80"); err == nil {
		t.Error("loopback target should be rejected")
	}
	if _, err := resolveSafe("localhost:80"); err == nil {
		t.Error("localhost (resolves to loopback) should be rejected")
	}
	if _, err := resolveSafe("no-port"); err == nil {
		t.Error("missing port should error")
	}
	if _, err := resolveSafe("nonexistent.invalid.:80"); err == nil {
		t.Error("unresolvable host should error")
	}
}

func TestSafeDialRejectsBlocked(t *testing.T) {
	if _, err := safeDial(context.Background(), "tcp", "169.254.169.254:80"); err == nil {
		t.Error("safeDial to metadata address should be rejected")
	}
}

func TestSafeDialReachesAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	// Relax the guard so the loopback test server counts as allowed, then dial it.
	orig := TargetAllowed
	TargetAllowed = func(net.IP) bool { return true }
	defer func() { TargetAllowed = orig }()

	conn, err := safeDial(context.Background(), "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("safeDial(allowed) = %v", err)
	}
	_ = conn.Close()
}
