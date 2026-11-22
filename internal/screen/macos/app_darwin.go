//go:build darwin && cgo

package macos

/*
#include <stdbool.h>
#include <CoreGraphics/CoreGraphics.h>
#include "native_darwin.h"
*/
import "C"

import (
	"log"
	"math"
	"os/exec"
	"sync"
	"unicode/utf16"

	"github.com/stonksfi/room-connector/internal/core"
	"github.com/stonksfi/room-connector/internal/screen/session"
)

// Run runs work on a goroutine while the calling goroutine, which must be
// main's and locked to the main thread, runs the app's event loop. It returns
// once work has.
func Run(work func()) {
	go func() {
		defer C.roomdAppStop()
		work()
	}()
	C.roomdAppRun()
}

// Permissions reports which of a view's permissions this process has.
func Permissions() core.ScreenPermissions {
	return core.ScreenPermissions{
		ScreenRecording: bool(C.CGPreflightScreenCaptureAccess()),
		Accessibility:   bool(C.CGPreflightPostEventAccess()),
	}
}

// RequestPermissions asks macOS for each missing permission, opens System
// Settings at the first one still missing, and reports what is granted.
func RequestPermissions() core.ScreenPermissions {
	p := Permissions()
	if !p.ScreenRecording {
		C.CGRequestScreenCaptureAccess()
	}
	if !p.Accessibility {
		C.CGRequestPostEventAccess()
	}
	p = Permissions()
	switch {
	case !p.ScreenRecording:
		openSettings("Privacy_ScreenCapture")
	case !p.Accessibility:
		openSettings("Privacy_Accessibility")
	}
	return p
}

func openSettings(pane string) {
	url := "x-apple.systempreferences:com.apple.preference.security?" + pane
	if err := exec.Command("/usr/bin/open", url).Run(); err != nil {
		log.Printf("open System Settings: %v", err)
	}
}

// Indicator is the menu bar's "Viewing" item: a session.Indicator.
type Indicator struct{}

var _ session.Indicator = Indicator{}

// onStopViewing is what the indicator's Stop viewing runs.
var onStopViewing struct {
	sync.Mutex
	f func()
}

func (Indicator) Show(onStop func()) {
	onStopViewing.Lock()
	onStopViewing.f = onStop
	onStopViewing.Unlock()
	C.roomdIndicatorShow()
}

func (Indicator) Hide() {
	onStopViewing.Lock()
	onStopViewing.f = nil
	onStopViewing.Unlock()
	C.roomdIndicatorHide()
}

//export roomdIndicatorStop
func roomdIndicatorStop() {
	onStopViewing.Lock()
	f := onStopViewing.f
	onStopViewing.Unlock()
	if f != nil {
		f()
	}
}

// Injector posts mouse and keyboard events at the HID level: a session.Injector.
type Injector struct{}

var _ session.Injector = Injector{}

// The keys the phone sends by name: a virtual keycode and the modifiers held.
var keys = map[session.SpecialKey]struct {
	code  C.uint16_t
	flags C.CGEventFlags
}{
	session.Backspace: {51, 0}, // delete
	session.Return:    {36, 0},
	session.ShiftTab:  {48, C.kCGEventFlagMaskShift}, // tab
}

// buttons maps each button to its CGEvent types.
var buttons = map[session.Button]struct {
	down, up, drag C.CGEventType
	button         C.CGMouseButton
}{
	session.Left:  {C.kCGEventLeftMouseDown, C.kCGEventLeftMouseUp, C.kCGEventLeftMouseDragged, C.kCGMouseButtonLeft},
	session.Right: {C.kCGEventRightMouseDown, C.kCGEventRightMouseUp, C.kCGEventRightMouseDragged, C.kCGMouseButtonRight},
}

func mouse(t C.CGEventType, p session.Point, b C.CGMouseButton, clicks int) {
	C.roomdMouse(C.int(t), C.double(p.X), C.double(p.Y), C.int(b), C.int(clicks))
}

func (Injector) MouseDown(p session.Point, b session.Button, clicks int) {
	e := buttons[b]
	mouse(e.down, p, e.button, clicks)
}

func (Injector) MouseUp(p session.Point, b session.Button, clicks int) {
	e := buttons[b]
	mouse(e.up, p, e.button, clicks)
}

func (Injector) MouseMove(p session.Point, held session.Button) {
	if e, ok := buttons[held]; ok {
		mouse(e.drag, p, e.button, 1)
		return
	}
	mouse(C.kCGEventMouseMoved, p, C.kCGMouseButtonLeft, 0)
}

func (Injector) Scroll(p session.Point, dx, dy float64) {
	// Scrolling goes to the window under the pointer, so bring it there first.
	mouse(C.kCGEventMouseMoved, p, C.kCGMouseButtonLeft, 0)
	wheel1, wheel2 := wheels(dx, dy)
	C.roomdScroll(C.double(p.X), C.double(p.Y), C.int32_t(wheel1), C.int32_t(wheel2))
}

// wheels turns the fingers' movement in points (positive right and down)
// into CG's pixel wheel deltas, so the content follows the fingers as with
// natural scrolling: a positive wheel1 moves the content down (revealing
// what is above), a positive wheel2 moves it right.
func wheels(dx, dy float64) (wheel1, wheel2 int32) {
	return int32(math.Round(dy)), int32(math.Round(dx))
}

// maxTypedUnits is how much text one keyboard event carries.
const maxTypedUnits = 20

func (Injector) Text(s string) {
	for _, chunk := range utf16Chunks(s, maxTypedUnits) {
		C.roomdType((*C.uint16_t)(&chunk[0]), C.int(len(chunk)))
	}
}

func (Injector) Key(k session.SpecialKey) {
	if key, ok := keys[k]; ok {
		C.roomdKey(key.code, C.uint64_t(key.flags))
	}
}

// utf16Chunks splits s into UTF-16 runs of at most max units, never inside
// a character.
func utf16Chunks(s string, max int) [][]uint16 {
	var out [][]uint16
	var chunk []uint16
	for _, r := range s {
		if len(chunk)+utf16.RuneLen(r) > max {
			out, chunk = append(out, chunk), nil
		}
		chunk = utf16.AppendRune(chunk, r)
	}
	if len(chunk) > 0 {
		out = append(out, chunk)
	}
	return out
}
