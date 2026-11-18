package vkcall

import (
	"fmt"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// NewPeerFactory returns a PeerFactory whose PeerConnections use a MediaEngine
// with pion's default codecs — VP8 (+RTX), H264 and Opus, the ones the VK SFU
// negotiates — and pion's default interceptors (RTCP sender/receiver reports,
// NACK, TWCC). The codecs are enough to answer the SFU's offer and, for Vector 2,
// to send VP8 (answering maps codecs by MIME type and adopts the offerer's PTs).
// The interceptors matter against a real SFU, which expects RR/NACK/TWCC feedback
// for media stability.
func NewPeerFactory() (PeerFactory, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("vkcall: register codecs: %w", err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return nil, fmt.Errorf("vkcall: register interceptors: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir))
	return func(cfg webrtc.Configuration) (*webrtc.PeerConnection, error) {
		return api.NewPeerConnection(cfg)
	}, nil
}
