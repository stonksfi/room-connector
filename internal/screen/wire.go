package screen

import "github.com/stonksfi/room-connector/internal/core"

// The roomd ↔ roomd-screen protocol, as JSON lines.
//
//	roomd-screen session  stdin:  one Request line, then held open: EOF means roomd is gone.
//	                      stdout: one Reply line; the process then serves the view and exits 0.
//	roomd-screen setup    stdout: one core.ScreenPermissions line.
//
// stderr carries log lines only: never SDP, never credentials.

// Request is the line roomd writes to `roomd-screen session`.
type Request struct {
	Offer core.ScreenOffer `json:"offer"`
}

// Reply is the line `roomd-screen session` answers: SDP or Unavailable.
type Reply struct {
	SDP         string      `json:"sdp,omitempty"`
	Unavailable Unavailable `json:"unavailable,omitempty"`
}

// Unavailable is why roomd-screen cannot start a view.
type Unavailable string

const (
	NeedsScreenRecording Unavailable = "needs_screen_recording"
	NeedsAccessibility   Unavailable = "needs_accessibility"
	Failed               Unavailable = "failed"
)
