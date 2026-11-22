// Package macos is the macOS Platform of a screen view, used only by
// roomd-screen: ScreenCaptureKit capture encoded by VideoToolbox, input posted
// with CGEventPost, the Screen Recording and Accessibility permissions, the
// menu bar indicator with the app run loop that owns the main thread, and
// the assertions that keep the display awake. It is cgo and Objective-C, and
// builds only on darwin with cgo: roomd itself never links it.
package macos
