package vkcall

import (
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/pristupaanastasia/turnvpn/internal/vchat"
)

func connInfo(t *testing.T) *vchat.ConnectionInfo {
	t.Helper()
	raw := []byte(`{"type":"notification","notification":"connection",
		"endpoint":"https://videowebrtc.okcdn.ru:23456/wt",
		"conversationParams":{
			"serverTime":123,
			"stun":{"urls":["stun:90.156.236.124:19302"]},
			"turn":{"urls":["turn:95.163.34.180:19302","turn:90.156.236.89:19302"],"username":"1795020784:590060927413","credential":"sec"}
		},
		"conversation":{"id":"conv-1","state":"ACTIVE","topology":"DIRECT"},
		"peerId":{"id":77,"type":"WEB_TRANSPORT"}}`)
	m, err := vchat.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	ci, ok := m.Connection()
	if !ok {
		t.Fatal("Connection() returned ok=false")
	}
	return ci
}

func TestConnectionInfoParsed(t *testing.T) {
	ci := connInfo(t)
	if ci.Conversation.ID != "conv-1" || ci.Conversation.Topology != "DIRECT" {
		t.Errorf("conversation: %+v", ci.Conversation)
	}
	if ci.PeerID.ID != 77 || ci.PeerID.Type != "WEB_TRANSPORT" {
		t.Errorf("peerId: %+v", ci.PeerID)
	}
	if len(ci.ConversationParams.Turn.URLs) != 2 || ci.ConversationParams.Turn.Username == "" {
		t.Errorf("turn: %+v", ci.ConversationParams.Turn)
	}
}

func TestICEServers(t *testing.T) {
	servers := ICEServers(connInfo(t))
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2 (stun+turn)", len(servers))
	}
	// First is STUN (no creds), second is TURN (with creds).
	if len(servers[0].URLs) != 1 || servers[0].Username != "" {
		t.Errorf("stun server: %+v", servers[0])
	}
	turn := servers[1]
	if len(turn.URLs) != 2 || turn.Username != "1795020784:590060927413" || turn.Credential != "sec" {
		t.Errorf("turn server: %+v", turn)
	}
	if turn.CredentialType != webrtc.ICECredentialTypePassword {
		t.Errorf("turn credential type = %v", turn.CredentialType)
	}
}

func TestICEServersNil(t *testing.T) {
	if ICEServers(nil) != nil {
		t.Error("ICEServers(nil) should be nil")
	}
	// Empty conversationParams yields no servers.
	if got := ICEServers(&vchat.ConnectionInfo{}); len(got) != 0 {
		t.Errorf("empty ci yields %d servers, want 0", len(got))
	}
}

func TestConfiguration(t *testing.T) {
	cfg := Configuration(connInfo(t))
	if len(cfg.ICEServers) != 2 {
		t.Errorf("configuration has %d ICE servers, want 2", len(cfg.ICEServers))
	}
}
