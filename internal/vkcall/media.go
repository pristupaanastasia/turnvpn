package vkcall

import (
	"fmt"

	"github.com/pion/webrtc/v4"
)

// NewPeerFactory returns a PeerFactory whose PeerConnections use a MediaEngine
// with pion's default codecs — which include VP8 (+RTX), H264 and Opus, the ones
// the VK SFU negotiates. That is enough to answer the SFU's offer and, for Vector
// 2, to send VP8. Answering maps codecs by MIME type and adopts the offerer's
// payload types, so the exact PTs need not match here.
func NewPeerFactory() (PeerFactory, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("vkcall: register codecs: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))
	return func(cfg webrtc.Configuration) (*webrtc.PeerConnection, error) {
		return api.NewPeerConnection(cfg)
	}, nil
}
