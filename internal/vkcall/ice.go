// Package vkcall wires the reversed signaling stack to a pion WebRTC peer: it
// turns the vchat "connection" notification into pion ICE configuration and (in
// later rungs) drives the offer/answer/ICE exchange to join a VK Звонки call as a
// native participant.
package vkcall

import (
	"github.com/pion/webrtc/v4"

	"github.com/pristupaanastasia/turnvpn/internal/vchat"
)

// ICEServers converts the STUN/TURN servers from the call's connection
// notification into pion ICE servers. The TURN credentials are the ephemeral ones
// the SFU issued for this session.
func ICEServers(ci *vchat.ConnectionInfo) []webrtc.ICEServer {
	if ci == nil {
		return nil
	}
	var out []webrtc.ICEServer
	if s := ci.ConversationParams.Stun; len(s.URLs) > 0 {
		out = append(out, webrtc.ICEServer{URLs: s.URLs})
	}
	if t := ci.ConversationParams.Turn; len(t.URLs) > 0 {
		out = append(out, webrtc.ICEServer{
			URLs:           t.URLs,
			Username:       t.Username,
			Credential:     t.Credential,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}
	return out
}

// Configuration builds a pion configuration for joining the call: the ICE servers
// from the connection notification. VK's SFU forbids peer-to-peer
// (p2p_forbidden), so media always goes through its relay.
func Configuration(ci *vchat.ConnectionInfo) webrtc.Configuration {
	return webrtc.Configuration{ICEServers: ICEServers(ci)}
}
