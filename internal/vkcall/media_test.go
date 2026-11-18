package vkcall

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestNewPeerFactoryVP8AndOpus(t *testing.T) {
	factory, err := NewPeerFactory()
	if err != nil {
		t.Fatal(err)
	}
	pc, err := factory(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	// Add a video and an audio transceiver and generate an offer; the codecs we
	// need for the call must be present.
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	sdp := offer.SDP
	for _, want := range []string{"VP8/90000", "opus/48000"} {
		if !strings.Contains(sdp, want) {
			t.Errorf("offer SDP missing %q", want)
		}
	}
}
