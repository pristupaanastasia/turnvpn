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

// TestNewPeerFactoryAnswersOfferWithVP8 exercises the real use: a factory peer
// answers an incoming VP8/opus offer and keeps those codecs in its answer.
func TestNewPeerFactoryAnswersOfferWithVP8(t *testing.T) {
	factory, err := NewPeerFactory()
	if err != nil {
		t.Fatal(err)
	}
	// An offerer with VP8+opus.
	offerer, _ := factory(webrtc.Configuration{})
	defer offerer.Close()
	offerer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	offerer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}

	// The answerer (what vkcall builds for the SFU) answers it.
	answerer, err := factory(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer answerer.Close()
	if err := answerer.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"VP8/90000", "opus/48000"} {
		if !strings.Contains(answer.SDP, want) {
			t.Errorf("answer SDP missing %q", want)
		}
	}
}
