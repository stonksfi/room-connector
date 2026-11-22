//go:build darwin && cgo

// Input injection, the accessory app's run loop and the menu bar indicator.

#import <AppKit/AppKit.h>
#import <CoreGraphics/CoreGraphics.h>

#include "_cgo_export.h"
#include "native_darwin.h"

static void post(CGEventRef e) {
    if (!e) return;
    CGEventPost(kCGHIDEventTap, e);
    CFRelease(e);
}

void roomdMouse(int type, double x, double y, int button, int clicks) {
    CGEventRef e = CGEventCreateMouseEvent(NULL, (CGEventType)type, CGPointMake(x, y), (CGMouseButton)button);
    if (e) CGEventSetIntegerValueField(e, kCGMouseEventClickState, clicks);
    post(e);
}

void roomdScroll(double x, double y, int32_t wheel1, int32_t wheel2) {
    CGEventRef e = CGEventCreateScrollWheelEvent2(NULL, kCGScrollEventUnitPixel, 2, wheel1, wheel2, 0);
    if (e) CGEventSetLocation(e, CGPointMake(x, y));
    post(e);
}

// keystroke posts a key down and up, typing units instead of the key's own
// character when count > 0.
static void keystroke(CGKeyCode code, CGEventFlags flags, const uint16_t *units, int count) {
    for (int down = 1; down >= 0; down--) {
        CGEventRef e = CGEventCreateKeyboardEvent(NULL, code, down);
        if (!e) continue;
        CGEventSetFlags(e, flags); // only these: never a modifier the person may be holding at the Mac
        if (count > 0) CGEventKeyboardSetUnicodeString(e, (UniCharCount)count, units);
        post(e);
    }
}

void roomdType(const uint16_t *units, int count) { keystroke(0, 0, units, count); }

void roomdKey(uint16_t keycode, uint64_t flags) { keystroke((CGKeyCode)keycode, (CGEventFlags)flags, NULL, 0); }

void roomdAppRun(void) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        [NSApp run];
    }
}

void roomdAppStop(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
      [NSApp stop:nil];
      // stop: takes effect once the run loop handles an event, so post one.
      NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined
                                         location:NSZeroPoint
                                    modifierFlags:0
                                        timestamp:0
                                     windowNumber:0
                                          context:nil
                                          subtype:0
                                            data1:0
                                            data2:0];
      [NSApp postEvent:wake atStart:YES];
    });
}

@interface RoomStopViewing : NSObject
- (void)stopViewing:(id)sender;
@end

@implementation RoomStopViewing
- (void)stopViewing:(id)sender {
    roomdIndicatorStop();
}
@end

// Touched only on the main thread.
static NSStatusItem *indicator;
static RoomStopViewing *stopTarget;

void roomdIndicatorShow(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
      if (indicator) return;
      stopTarget = [RoomStopViewing new];
      indicator = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
      indicator.button.image = [NSImage imageWithSystemSymbolName:@"iphone" accessibilityDescription:@"Room"];
      indicator.button.imagePosition = NSImageLeading;
      indicator.button.title = @"Viewing";
      indicator.button.toolTip = @"Room: your iPhone is viewing this screen";

      NSMenu *menu = [NSMenu new];
      [menu addItem:[[NSMenuItem alloc] initWithTitle:@"Your iPhone is viewing this screen" action:nil keyEquivalent:@""]];
      [menu addItem:[NSMenuItem separatorItem]];
      NSMenuItem *stop = [[NSMenuItem alloc] initWithTitle:@"Stop viewing" action:@selector(stopViewing:) keyEquivalent:@""];
      stop.target = stopTarget;
      [menu addItem:stop];
      indicator.menu = menu;
    });
}

void roomdIndicatorHide(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
      if (!indicator) return;
      [[NSStatusBar systemStatusBar] removeStatusItem:indicator];
      indicator = nil;
    });
}
