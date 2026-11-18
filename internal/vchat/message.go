// Package vchat implements the OK "vchat" signaling message protocol that VK
// Звонки runs over the WebTransport stream (framing handled by wtframe/wtsignal).
// Reverse-engineered from live capture (2026-11-18); see
// tools/fbdo-capture/signaling-reference.md.
//
// Messages are JSON objects, or the bare control strings "ping"/"pong". The
// client sends commands {command, sequence, ...}; the server sends notifications
// {type:"notification", notification, ...} and responses {type:"response",
// response, sequence}. WebRTC offers/answers and ICE candidates ride inside a
// transmit-data command / transmitted-data notification as plaintext JSON — no
// base64.
package vchat

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ParticipantUser is the participantType used for user-to-user media signaling.
const ParticipantUser = "USER"

// SDP is a WebRTC session description (offer/answer) carried in transmit-data.
type SDP struct {
	Type string `json:"type"` // "offer" or "answer"
	SDP  string `json:"sdp"`
}

// Candidate is a trickled ICE candidate.
type Candidate struct {
	Candidate        string `json:"candidate"`
	SDPMid           string `json:"sdpMid"`
	SDPMLineIndex    int    `json:"sdpMLineIndex"`
	UsernameFragment string `json:"usernameFragment,omitempty"`
}

// transmitPayload is the data union of a transmit-data message: an SDP, or an ICE
// candidate.
type transmitPayload struct {
	SDP            *SDP       `json:"sdp,omitempty"`
	Candidate      *Candidate `json:"candidate,omitempty"`
	AnimojiVersion *int       `json:"animojiVersion,omitempty"`
}

// command is the outgoing envelope.
type command struct {
	Command         string           `json:"command"`
	Sequence        int              `json:"sequence"`
	ParticipantID   *int64           `json:"participantId,omitempty"`
	ParticipantType string           `json:"participantType,omitempty"`
	Data            *transmitPayload `json:"data,omitempty"`
}

// TransmitSDP builds a transmit-data command carrying an offer or answer to the
// given participant.
func TransmitSDP(sequence int, participantID int64, sdp SDP) ([]byte, error) {
	return json.Marshal(command{
		Command:         "transmit-data",
		Sequence:        sequence,
		ParticipantID:   &participantID,
		ParticipantType: ParticipantUser,
		Data:            &transmitPayload{SDP: &sdp},
	})
}

// TransmitCandidate builds a transmit-data command carrying a trickled ICE
// candidate to the given participant.
func TransmitCandidate(sequence int, participantID int64, c Candidate) ([]byte, error) {
	return json.Marshal(command{
		Command:         "transmit-data",
		Sequence:        sequence,
		ParticipantID:   &participantID,
		ParticipantType: ParticipantUser,
		Data:            &transmitPayload{Candidate: &c},
	})
}

// Kind classifies a parsed inbound message.
type Kind int

const (
	KindUnknown Kind = iota
	KindNotification
	KindResponse
	KindControl // ping/pong
)

// Message is a parsed inbound signaling message.
type Message struct {
	Kind            Kind
	Type            string // raw "type" field
	Notification    string // for notifications, the "notification" name
	Response        string // for responses, the acknowledged command name
	Sequence        int
	Stamp           int64
	Control         string // "ping" or "pong" for KindControl
	ParticipantID   int64
	ParticipantType string
	Data            json.RawMessage // raw "data" (transmit payload, participant data, …)
}

// Parse decodes one inbound message: the control strings "ping"/"pong", or a JSON
// envelope classified by its "type".
func Parse(raw []byte) (Message, error) {
	t := bytes.TrimSpace(raw)
	switch string(t) {
	case "ping":
		return Message{Kind: KindControl, Control: "ping"}, nil
	case "pong":
		return Message{Kind: KindControl, Control: "pong"}, nil
	}
	var env struct {
		Type            string          `json:"type"`
		Notification    string          `json:"notification"`
		Response        string          `json:"response"`
		Sequence        int             `json:"sequence"`
		Stamp           int64           `json:"stamp"`
		ParticipantID   int64           `json:"participantId"`
		ParticipantType string          `json:"participantType"`
		Data            json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(t, &env); err != nil {
		return Message{}, fmt.Errorf("vchat: parse: %w", err)
	}
	m := Message{
		Type:            env.Type,
		Notification:    env.Notification,
		Response:        env.Response,
		Sequence:        env.Sequence,
		Stamp:           env.Stamp,
		ParticipantID:   env.ParticipantID,
		ParticipantType: env.ParticipantType,
		Data:            env.Data,
	}
	switch env.Type {
	case "notification":
		m.Kind = KindNotification
	case "response":
		m.Kind = KindResponse
	default:
		m.Kind = KindUnknown
	}
	return m, nil
}

// IsPing reports whether the message is a server heartbeat that must be answered
// with Pong.
func (m Message) IsPing() bool { return m.Kind == KindControl && m.Control == "ping" }

// Pong is the heartbeat reply to a ping.
func Pong() []byte { return []byte("pong") }

// TransmittedData reports whether this is a "transmitted-data" notification (peer
// SDP or ICE) and, if so, decodes its payload.
func (m Message) TransmittedData() (sdp *SDP, cand *Candidate, ok bool) {
	if m.Kind != KindNotification || m.Notification != "transmitted-data" || len(m.Data) == 0 {
		return nil, nil, false
	}
	var p transmitPayload
	if err := json.Unmarshal(m.Data, &p); err != nil {
		return nil, nil, false
	}
	// ok only when a payload is actually present, so a caller can safely
	// dereference the returned pointer (data may be null or carry neither).
	return p.SDP, p.Candidate, p.SDP != nil || p.Candidate != nil
}

// IsConnection reports whether this is the join-confirmation "connection"
// notification (which also carries conversationParams with STUN/TURN).
func (m Message) IsConnection() bool {
	return m.Kind == KindNotification && m.Notification == "connection"
}
