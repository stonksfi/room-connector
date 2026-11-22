package core

import (
	"context"
	"errors"
)

// ScreenOffer is a viewer's request to see and control this machine's screen:
// a complete (non-trickle) WebRTC offer and the ICE servers to answer it with.
// The SDP holds network addresses and the ICE servers hold TURN credentials,
// so neither is ever logged; String names only the offer.
type ScreenOffer struct {
	ID         string      `json:"id"`
	SDP        string      `json:"sdp"`
	ICEServers []ICEServer `json:"ice_servers,omitempty"`
}

func (o ScreenOffer) String() string { return "screen offer " + o.ID }

// ICEServer is one STUN or TURN server. Credential is secret.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

func (s ICEServer) String() string { return "ice server" }

// MaxAnswerSDP is the largest answer SDP Room's API accepts.
const MaxAnswerSDP = 64 << 10

// ScreenPermissions is which macOS permissions a screen view needs are granted.
type ScreenPermissions struct {
	ScreenRecording bool `json:"screen_recording"`
	Accessibility   bool `json:"accessibility"`
}

// A screen view cannot start until the person grants the permission at the Mac.
var (
	ErrNeedsScreenRecording = errors.New("screen recording is not allowed for roomd: run `roomd screen setup`")
	ErrNeedsAccessibility   = errors.New("accessibility is not allowed for roomd: run `roomd screen setup`")
)

// ScreenHost runs screen views of this machine, at most one at a time.
type ScreenHost interface {
	// Answer starts a view for offer, replacing any running one, and returns
	// its answer SDP or ErrNeedsScreenRecording / ErrNeedsAccessibility. The
	// view runs until ctx ends, Stop is called, the next Answer replaces it or
	// either side ends it.
	Answer(ctx context.Context, offer ScreenOffer) (string, error)
	// Stop ends the running view, if any.
	Stop()
}
