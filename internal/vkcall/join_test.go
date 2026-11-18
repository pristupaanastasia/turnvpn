package vkcall

import (
	"context"
	"testing"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/vchat"
)

func connMsg(t *testing.T) vchat.Message {
	t.Helper()
	m, err := vchat.Parse([]byte(`{"type":"notification","notification":"connection",
		"conversationParams":{"stun":{"urls":["stun:1.2.3.4:19302"]},"turn":{"urls":["turn:1.2.3.4:19302"],"username":"u","credential":"c"}},
		"conversation":{"id":"c1"},"peerId":{"id":1,"type":"WEB_TRANSPORT"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAwaitConnectionSkipsEarlierEvents(t *testing.T) {
	ev := make(chan vchat.Message, 4)
	// A couple of non-connection events, then the connection notification.
	m1, _ := vchat.Parse([]byte(`{"type":"response","response":"x","sequence":1}`))
	ev <- m1
	ev <- msgCandidate("early")
	ev <- connMsg(t)

	ci, err := awaitConnection(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if ci == nil || ci.Conversation.ID != "c1" || len(ci.ConversationParams.Turn.URLs) != 1 {
		t.Errorf("connection info: %+v", ci)
	}
	if len(ICEServers(ci)) != 2 {
		t.Errorf("expected stun+turn servers, got %d", len(ICEServers(ci)))
	}
}

func TestAwaitConnectionStreamEnds(t *testing.T) {
	ev := make(chan vchat.Message)
	close(ev)
	if _, err := awaitConnection(context.Background(), ev); err == nil {
		t.Error("expected error when stream ends before connection")
	}
}

func TestAwaitConnectionContextCancel(t *testing.T) {
	ev := make(chan vchat.Message) // nothing ever arrives
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := awaitConnection(ctx, ev); err == nil {
		t.Error("expected ctx error")
	}
}
