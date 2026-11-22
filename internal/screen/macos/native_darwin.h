//go:build darwin && cgo

// The Objective-C half of package macos. Every function here is safe to call
// from any thread unless it says otherwise.
#ifndef ROOMD_NATIVE_DARWIN_H
#define ROOMD_NATIVE_DARWIN_H

#include <stdbool.h>
#include <stdint.h>

// Capture: an SCStream of the main display feeding a VTCompressionSession.
// Frames and the first error are reported to roomdCaptureFrame and
// roomdCaptureFailed (Go) with handle; neither is called once
// roomdCaptureStop returns.
typedef void *RoomCapture;
void roomdMainDisplayPixels(int *width, int *height);
// Returns NULL and sets *err (free it) on failure. Without screen, only the
// encoder runs, fed by roomdCapturePush: tests check it without permissions.
RoomCapture roomdCaptureStart(uintptr_t handle, int width, int height, int bitrate, bool screen, char **err);
void roomdCaptureStop(RoomCapture c);
void roomdCaptureKeyframe(RoomCapture c);
void roomdCaptureSetBitrate(RoomCapture c, int bitrate);
// Encodes a synthetic frame of one shade.
void roomdCapturePush(RoomCapture c, int64_t ptsNanos, uint8_t shade);

// Input, posted at the HID level. Points are global display points.
void roomdMouse(int type, double x, double y, int button, int clicks);
// wheel1 is vertical and wheel2 horizontal, in CG's signs (see wheels).
void roomdScroll(double x, double y, int32_t wheel1, int32_t wheel2);
void roomdType(const uint16_t *units, int count);
// flags are the modifiers held for the key press, and only those.
void roomdKey(uint16_t keycode, uint64_t flags);

// The app: an accessory NSApplication whose run loop owns the main thread.
// roomdAppRun must be called on the main thread; it returns after roomdAppStop.
void roomdAppRun(void);
void roomdAppStop(void);
// The menu bar indicator; Stop viewing calls roomdIndicatorStop (Go).
void roomdIndicatorShow(void);
void roomdIndicatorHide(void);

#endif
