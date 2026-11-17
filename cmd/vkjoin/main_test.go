package main

import (
	"testing"

	"github.com/pristupaanastasia/turnvpn/internal/okapi"
)

func TestBuildParams(t *testing.T) {
	login := &okapi.AnonymLoginResult{UID: "12345"}
	desc := &okapi.Descriptor{
		ID:         "conv-99",
		DeviceIdx:  3,
		Token:      "tok",
		Endpoint:   "wss://videowebrtc.okcdn.ru/ws2",
		WtEndpoint: "https://videowebrtc.okcdn.ru:23456/wt",
	}
	cfg := config{clientVersion: "1.2.3", deviceID: "dev-1", entityType: "GROUP"}

	p, useWT, err := buildParams(login, desc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !useWT {
		t.Error("useWT should be true when wt_endpoint is present")
	}
	if p.UserID != 12345 {
		t.Errorf("UserID = %d, want 12345", p.UserID)
	}
	if p.DeviceIdx != 3 || p.ConversationID != "conv-99" || p.Token != "tok" {
		t.Errorf("identity mapping wrong: %+v", p)
	}
	if p.EntityType != "GROUP" || p.ClientType != okapi.WebClientType || p.Capabilities != okapi.DefaultCapabilities {
		t.Errorf("client mapping wrong: %+v", p)
	}
}

func TestBuildParamsNoWebTransport(t *testing.T) {
	login := &okapi.AnonymLoginResult{UID: "1"}
	desc := &okapi.Descriptor{Endpoint: "wss://h/ws2"} // no wt_endpoint
	_, useWT, err := buildParams(login, desc, config{})
	if err != nil {
		t.Fatal(err)
	}
	if useWT {
		t.Error("useWT should be false without wt_endpoint")
	}
}

func TestBuildParamsBadUID(t *testing.T) {
	// A non-numeric uid is tolerated: proceed with userId=0 rather than abort.
	login := &okapi.AnonymLoginResult{UID: "not-a-number"}
	p, _, err := buildParams(login, &okapi.Descriptor{}, config{})
	if err != nil {
		t.Fatalf("non-numeric uid should not error: %v", err)
	}
	if p.UserID != 0 {
		t.Errorf("UserID = %d, want 0 for bad uid", p.UserID)
	}
}

func TestBuildParamsEmptyUID(t *testing.T) {
	// An empty uid is tolerated (UserID 0), not an error.
	login := &okapi.AnonymLoginResult{UID: ""}
	p, _, err := buildParams(login, &okapi.Descriptor{}, config{})
	if err != nil {
		t.Fatalf("empty uid should not error: %v", err)
	}
	if p.UserID != 0 {
		t.Errorf("UserID = %d, want 0", p.UserID)
	}
}
