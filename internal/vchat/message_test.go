package vchat

import (
	"encoding/json"
	"testing"
)

func TestTransmitSDP(t *testing.T) {
	raw, err := TransmitSDP(5, 42, SDP{Type: "answer", SDP: "v=0..."})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Command         string `json:"command"`
		Sequence        int    `json:"sequence"`
		ParticipantID   int64  `json:"participantId"`
		ParticipantType string `json:"participantType"`
		Data            struct {
			SDP *SDP `json:"sdp"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Command != "transmit-data" || got.Sequence != 5 || got.ParticipantID != 42 || got.ParticipantType != "USER" {
		t.Errorf("envelope wrong: %+v", got)
	}
	if got.Data.SDP == nil || got.Data.SDP.Type != "answer" || got.Data.SDP.SDP != "v=0..." {
		t.Errorf("sdp wrong: %+v", got.Data.SDP)
	}
}

func TestTransmitCandidate(t *testing.T) {
	raw, err := TransmitCandidate(7, 42, Candidate{Candidate: "candidate:1 ...", SDPMid: "0", SDPMLineIndex: 1, UsernameFragment: "UZ8l"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Command string `json:"command"`
		Data    struct {
			Candidate *Candidate `json:"candidate"`
			SDP       *SDP       `json:"sdp"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Command != "transmit-data" || got.Data.SDP != nil {
		t.Errorf("should carry only a candidate: %+v", got)
	}
	if got.Data.Candidate == nil || got.Data.Candidate.SDPMid != "0" || got.Data.Candidate.UsernameFragment != "UZ8l" {
		t.Errorf("candidate wrong: %+v", got.Data.Candidate)
	}
}

func TestParsePingPong(t *testing.T) {
	m, _ := Parse([]byte("ping"))
	if !m.IsPing() {
		t.Error("ping not recognized")
	}
	if string(Pong()) != "pong" {
		t.Error("Pong() wrong")
	}
	m2, _ := Parse([]byte("pong"))
	if m2.Kind != KindControl || m2.Control != "pong" {
		t.Errorf("pong parse: %+v", m2)
	}
}

func TestParseOfferNotification(t *testing.T) {
	raw := []byte(`{"type":"notification","notification":"transmitted-data","stamp":123,"participantId":99,"participantType":"USER","data":{"sdp":{"type":"offer","sdp":"v=0\r\nm=audio..."},"animojiVersion":2}}`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindNotification || m.Notification != "transmitted-data" {
		t.Fatalf("kind/notification: %+v", m)
	}
	if m.ParticipantID != 99 {
		t.Errorf("participantId = %d", m.ParticipantID)
	}
	sdp, cand, ok := m.TransmittedData()
	if !ok || cand != nil || sdp == nil || sdp.Type != "offer" {
		t.Errorf("TransmittedData sdp: %v %v %v", sdp, cand, ok)
	}
}

func TestParseCandidateNotification(t *testing.T) {
	raw := []byte(`{"type":"notification","notification":"transmitted-data","data":{"candidate":{"candidate":"candidate:1 1 udp","sdpMid":"0","sdpMLineIndex":0,"usernameFragment":"gzxK"}}}`)
	m, _ := Parse(raw)
	sdp, cand, ok := m.TransmittedData()
	if !ok || sdp != nil || cand == nil || cand.UsernameFragment != "gzxK" {
		t.Errorf("TransmittedData candidate: %v %v %v", sdp, cand, ok)
	}
}

func TestTransmittedDataEmptyPayload(t *testing.T) {
	// A transmitted-data whose data has neither sdp nor candidate must report
	// ok=false so a caller never dereferences a nil pointer.
	for _, raw := range []string{
		`{"type":"notification","notification":"transmitted-data","data":{"animojiVersion":2}}`,
		`{"type":"notification","notification":"transmitted-data","data":null}`,
		`{"type":"notification","notification":"transmitted-data"}`,
	} {
		m, _ := Parse([]byte(raw))
		if sdp, cand, ok := m.TransmittedData(); ok || sdp != nil || cand != nil {
			t.Errorf("%s: want ok=false/nil, got sdp=%v cand=%v ok=%v", raw, sdp, cand, ok)
		}
	}
}

func TestParseResponse(t *testing.T) {
	raw := []byte(`{"type":"response","response":"update-media-modifiers","sequence":3,"stamp":456}`)
	m, _ := Parse(raw)
	if m.Kind != KindResponse || m.Response != "update-media-modifiers" || m.Sequence != 3 {
		t.Errorf("response parse: %+v", m)
	}
}

func TestParseConnection(t *testing.T) {
	raw := []byte(`{"type":"notification","notification":"connection","conversation":{"state":"ACTIVE"}}`)
	m, _ := Parse(raw)
	if !m.IsConnection() {
		t.Errorf("IsConnection false: %+v", m)
	}
	// A non-connection notification is not a connection.
	m2, _ := Parse([]byte(`{"type":"notification","notification":"transmitted-data"}`))
	if m2.IsConnection() {
		t.Error("transmitted-data misreported as connection")
	}
}

func TestParseInvalid(t *testing.T) {
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Error("expected error for non-JSON, non-control input")
	}
}
