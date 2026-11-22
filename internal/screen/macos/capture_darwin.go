//go:build darwin && cgo

package macos

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework AppKit -framework CoreGraphics -framework CoreMedia -framework CoreVideo -framework IOKit -framework ScreenCaptureKit -framework VideoToolbox
#include <stdbool.h>
#include <stdlib.h>
#include <CoreGraphics/CoreGraphics.h>
#include "native_darwin.h"
*/
import "C"

import (
	"errors"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"

	"github.com/stonksfi/room-connector/internal/screen/session"
)

// maxLongSide caps the capture: full resolution keeps zoomed text readable,
// and more than this is past what a phone decodes smoothly.
const maxLongSide = 2560

// CaptureSize is the size the main display is captured at, in pixels.
func CaptureSize() (w, h int) {
	var pw, ph C.int
	C.roomdMainDisplayPixels(&pw, &ph)
	return session.FitLongSide(int(pw), int(ph), maxLongSide)
}

// Capture is the main display, encoded as H.264: a session.Source.
type Capture struct {
	mu      sync.Mutex
	native  C.RoomCapture // nil unless capturing
	handle  cgo.Handle
	bitrate int
}

var _ session.Source = (*Capture)(nil)

// callbacks is what the Objective-C side reaches through a cgo.Handle.
type callbacks struct {
	onFrame func(session.Frame)
	onError func(error)
}

func NewCapture() *Capture { return &Capture{bitrate: 2_500_000} }

func (c *Capture) Bounds() session.Rect {
	b := C.CGDisplayBounds(C.CGMainDisplayID())
	return session.Rect{X: float64(b.origin.x), Y: float64(b.origin.y), W: float64(b.size.width), H: float64(b.size.height)}
}

func (c *Capture) Start(onFrame func(session.Frame), onError func(error)) error {
	w, h := CaptureSize()
	return c.start(w, h, true, onFrame, onError)
}

// start runs the encoder at w×h, fed by the screen or, without it, by push.
func (c *Capture) start(w, h int, screen bool, onFrame func(session.Frame), onError func(error)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.native != nil {
		return errors.New("already capturing")
	}
	handle := cgo.NewHandle(&callbacks{onFrame: onFrame, onError: onError})
	var cerr *C.char
	native := C.roomdCaptureStart(C.uintptr_t(handle), C.int(w), C.int(h), C.int(c.bitrate), C.bool(screen), &cerr)
	if native == nil {
		handle.Delete()
		defer C.free(unsafe.Pointer(cerr))
		return errors.New(C.GoString(cerr))
	}
	c.native, c.handle = native, handle
	return nil
}

func (c *Capture) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.native == nil {
		return
	}
	C.roomdCaptureStop(c.native) // no callback runs after this returns
	c.handle.Delete()
	c.native = nil
}

func (c *Capture) ForceKeyframe() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.native != nil {
		C.roomdCaptureKeyframe(c.native)
	}
}

func (c *Capture) SetBitrate(bps int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bitrate = bps
	if c.native != nil {
		C.roomdCaptureSetBitrate(c.native, C.int(bps))
	}
}

//export roomdCaptureFrame
func roomdCaptureFrame(handle C.uintptr_t, data unsafe.Pointer, length C.int, ptsNanos C.int64_t, keyframe C.int, encodeNanos C.int64_t) {
	cgo.Handle(handle).Value().(*callbacks).onFrame(session.Frame{
		Data:     C.GoBytes(data, length),
		PTS:      time.Duration(ptsNanos),
		Keyframe: keyframe != 0,
		Encode:   time.Duration(encodeNanos),
	})
}

//export roomdCaptureFailed
func roomdCaptureFailed(handle C.uintptr_t, message *C.char) {
	cgo.Handle(handle).Value().(*callbacks).onError(errors.New(C.GoString(message)))
}

// push encodes a synthetic frame of one shade, for tests of a capture
// started without the screen.
func (c *Capture) push(pts time.Duration, shade uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	C.roomdCapturePush(c.native, C.int64_t(pts), C.uint8_t(shade))
}
