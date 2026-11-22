//go:build darwin && cgo

// Capture: ScreenCaptureKit frames of the main display, encoded by
// VideoToolbox as H.264 constrained baseline in Annex-B.

#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <Foundation/Foundation.h>
#import <IOKit/pwr_mgt/IOPMLib.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <VideoToolbox/VideoToolbox.h>
#include <string.h>
#include <time.h>

#include "_cgo_export.h"
#include "native_darwin.h"

static const uint8_t startCode[4] = {0, 0, 0, 1};
static const int64_t waitSeconds = 10;
// While frames arrive at least this often the screen is moving, and a
// keyframe request waits for the next live frame instead of re-encoding.
static const int64_t liveWindowNanos = 66 * NSEC_PER_MSEC; // two frames at 30 fps

static int64_t nowNanos(void) { return (int64_t)clock_gettime_nsec_np(CLOCK_UPTIME_RAW); }

static OSStatus setProperty(VTCompressionSessionRef s, CFStringRef key, CFTypeRef value) {
    OSStatus st = VTSessionSetProperty(s, key, value);
    if (st != noErr) {
        fprintf(stderr, "roomd-screen: encoder property %s: %d\n",
                CFStringGetCStringPtr(key, kCFStringEncodingUTF8) ?: "?", (int)st);
    }
    return st;
}

static void encoded(void *ref, void *frameRef, OSStatus status, VTEncodeInfoFlags flags, CMSampleBufferRef sb);

@interface RoomCaptureImpl : NSObject <SCStreamOutput, SCStreamDelegate> {
@public
    uintptr_t _handle;
    int _width, _height;
    IOPMAssertionID _activity, _awake; // kIOPMNullAssertionID unless held
    // The display list, requested before the encoder is built.
    dispatch_semaphore_t _contentReady;
    SCShareableContent *_content;
    NSError *_contentErr;
    // Everything below is touched only on _queue, except _session's
    // callbacks, which VideoToolbox runs on its own thread.
    dispatch_queue_t _queue;
    SCStream *_stream;
    VTCompressionSessionRef _session;
    CVPixelBufferRef _last; // re-encoded to answer a keyframe request on a still screen
    int64_t _lastFrameNanos;
    CMTime _lastPTS;
    BOOL _stopped, _failed, _keyframePending, _rateLimitBroken;
}
@end

@implementation RoomCaptureImpl

- (instancetype)initWithHandle:(uintptr_t)handle width:(int)width height:(int)height {
    if ((self = [super init])) {
        _handle = handle;
        _width = width;
        _height = height;
        _queue = dispatch_queue_create("com.stonksfi.roomd-screen.capture", DISPATCH_QUEUE_SERIAL);
        _lastPTS = kCMTimeInvalid;
    }
    return self;
}

- (NSString *)openEncoderWithBitrate:(int)bitrate {
    NSDictionary *lowLatency = @{(__bridge NSString *)kVTVideoEncoderSpecification_EnableLowLatencyRateControl : @YES};
    OSStatus st = VTCompressionSessionCreate(NULL, _width, _height, kCMVideoCodecType_H264,
                                             (__bridge CFDictionaryRef)lowLatency, NULL, NULL, encoded,
                                             (__bridge void *)self, &_session);
    if (st != noErr) {
        fprintf(stderr, "roomd-screen: no low-latency H.264 encoder (%d), using the default one\n", (int)st);
        st = VTCompressionSessionCreate(NULL, _width, _height, kCMVideoCodecType_H264, NULL, NULL, NULL, encoded,
                                        (__bridge void *)self, &_session);
    }
    if (st != noErr) {
        _session = NULL;
        return [NSString stringWithFormat:@"create the H.264 encoder: %d", (int)st];
    }
    setProperty(_session, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
    setProperty(_session, kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel);
    setProperty(_session, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse);
    setProperty(_session, kVTCompressionPropertyKey_ExpectedFrameRate, (__bridge CFTypeRef) @30);
    setProperty(_session, kVTCompressionPropertyKey_ColorPrimaries, kCVImageBufferColorPrimaries_ITU_R_709_2);
    setProperty(_session, kVTCompressionPropertyKey_TransferFunction, kCVImageBufferTransferFunction_ITU_R_709_2);
    setProperty(_session, kVTCompressionPropertyKey_YCbCrMatrix, kCVImageBufferYCbCrMatrix_ITU_R_709_2);
    [self applyBitrate:bitrate];
    st = VTCompressionSessionPrepareToEncodeFrames(_session);
    if (st != noErr) fprintf(stderr, "roomd-screen: prepare the encoder: %d (it prepares on the first frame)\n", (int)st);
    return nil;
}

// applyBitrate runs on _queue (or before capture starts).
- (void)applyBitrate:(int)bitrate {
    if (!_session) return;
    setProperty(_session, kVTCompressionPropertyKey_AverageBitRate, (__bridge CFTypeRef) @(bitrate));
    if (_rateLimitBroken) return;
    // A hard cap 50% over the target within any one second.
    NSArray *limit = @[ @((double)bitrate * 1.5 / 8.0), @1.0 ];
    if (setProperty(_session, kVTCompressionPropertyKey_DataRateLimits, (__bridge CFTypeRef)limit) != noErr) {
        _rateLimitBroken = YES; // say so once, not every second
    }
}

// encode runs on _queue.
- (void)encode:(CVPixelBufferRef)pb at:(CMTime)pts keyframe:(BOOL)keyframe {
    if (!_session) return;
    if (CMTIME_IS_VALID(_lastPTS) && CMTimeCompare(pts, _lastPTS) <= 0) {
        pts = CMTimeAdd(_lastPTS, CMTimeMake(1, 1000)); // the encoder needs time to move forward
    }
    _lastPTS = pts;
    NSDictionary *opts = keyframe ? @{(__bridge NSString *)kVTEncodeFrameOptionKey_ForceKeyFrame : @YES} : nil;
    OSStatus st = VTCompressionSessionEncodeFrame(_session, pb, pts, kCMTimeInvalid, (__bridge CFDictionaryRef)opts,
                                                  (void *)(intptr_t)nowNanos(), NULL);
    if (st != noErr) fprintf(stderr, "roomd-screen: encode a frame: %d\n", (int)st);
}

// frame encodes a new picture of the screen, as the keyframe if one is owed,
// and keeps it for a still screen's keyframe requests. It runs on _queue.
- (void)frame:(CVPixelBufferRef)pb at:(CMTime)pts {
    [self encode:pb at:pts keyframe:_keyframePending];
    _keyframePending = NO;
    _lastFrameNanos = nowNanos();
    CVPixelBufferRetain(pb);
    if (_last) CVPixelBufferRelease(_last);
    _last = pb;
}

// encodePendingKeyframe re-encodes the last picture as the keyframe still
// owed: on a still screen no new frame would carry it. It runs on _queue.
- (void)encodePendingKeyframe {
    if (_stopped || !_keyframePending || !_last) return;
    _keyframePending = NO;
    [self encode:_last at:CMClockGetTime(CMClockGetHostTimeClock()) keyframe:YES];
}

- (void)holdDisplayAwake {
    // Declaring activity wakes a sleeping display; the assertion keeps it on.
    IOReturn r = IOPMAssertionDeclareUserActivity(CFSTR("Room screen view"), kIOPMUserActiveLocal, &_activity);
    if (r != kIOReturnSuccess) {
        fprintf(stderr, "roomd-screen: wake the display: 0x%x\n", r);
        _activity = kIOPMNullAssertionID;
    }
    r = IOPMAssertionCreateWithName(kIOPMAssertionTypePreventUserIdleDisplaySleep, kIOPMAssertionLevelOn,
                                    CFSTR("Room screen view"), &_awake);
    if (r != kIOReturnSuccess) {
        fprintf(stderr, "roomd-screen: keep the display awake: 0x%x (it may sleep during the view)\n", r);
        _awake = kIOPMNullAssertionID;
    }
}

// requestContent starts listing the displays, a slow cross-process call, so
// it overlaps building the encoder; startStream waits for it.
- (void)requestContent {
    _contentReady = dispatch_semaphore_create(0);
    [SCShareableContent getShareableContentWithCompletionHandler:^(SCShareableContent *c, NSError *e) {
      self->_content = c;
      self->_contentErr = e;
      dispatch_semaphore_signal(self->_contentReady);
    }];
}

- (NSString *)startStream {
    if (dispatch_semaphore_wait(_contentReady, dispatch_time(DISPATCH_TIME_NOW, waitSeconds * NSEC_PER_SEC)) != 0) {
        return @"list the displays: timed out";
    }
    if (!_content) return [NSString stringWithFormat:@"list the displays: %@", _contentErr.localizedDescription];
    SCDisplay *display = nil;
    for (SCDisplay *d in _content.displays) {
        if (d.displayID == CGMainDisplayID()) display = d;
    }
    if (!display) return @"the main display cannot be captured";

    SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:display excludingWindows:@[]];
    SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
    cfg.width = (size_t)_width;
    cfg.height = (size_t)_height;
    cfg.minimumFrameInterval = CMTimeMake(1, 30);
    cfg.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange; // 420v
    cfg.colorMatrix = kCVImageBufferYCbCrMatrix_ITU_R_709_2;
    cfg.showsCursor = YES;
    cfg.queueDepth = 3;
    _stream = [[SCStream alloc] initWithFilter:filter configuration:cfg delegate:self];
    NSError *addErr = nil;
    if (![_stream addStreamOutput:self type:SCStreamOutputTypeScreen sampleHandlerQueue:_queue error:&addErr]) {
        return [NSString stringWithFormat:@"capture the display: %@", addErr.localizedDescription];
    }
    dispatch_semaphore_t started = dispatch_semaphore_create(0);
    __block NSError *startErr = nil;
    [_stream startCaptureWithCompletionHandler:^(NSError *e) {
      startErr = e;
      dispatch_semaphore_signal(started);
    }];
    if (dispatch_semaphore_wait(started, dispatch_time(DISPATCH_TIME_NOW, waitSeconds * NSEC_PER_SEC)) != 0) {
        return @"start capture: timed out";
    }
    if (startErr) return [NSString stringWithFormat:@"start capture: %@", startErr.localizedDescription];
    return nil;
}

- (void)stream:(SCStream *)stream didOutputSampleBuffer:(CMSampleBufferRef)sb ofType:(SCStreamOutputType)type {
    @autoreleasepool {
        if (type != SCStreamOutputTypeScreen || _stopped || !CMSampleBufferIsValid(sb)) return;
        CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sb, false);
        if (!attachments || CFArrayGetCount(attachments) == 0) return;
        NSDictionary *info = (__bridge NSDictionary *)CFArrayGetValueAtIndex(attachments, 0);
        NSNumber *status = info[SCStreamFrameInfoStatus];
        // Idle, blank and suspended frames carry nothing new: skip them.
        if (!status || (status.integerValue != SCFrameStatusComplete && status.integerValue != SCFrameStatusStarted)) {
            return;
        }
        CVPixelBufferRef pb = CMSampleBufferGetImageBuffer(sb);
        if (pb) [self frame:pb at:CMSampleBufferGetPresentationTimeStamp(sb)];
    }
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
    NSString *message = error.localizedDescription ?: @"capture stopped";
    dispatch_async(_queue, ^{
      if (self->_stopped || self->_failed) return;
      self->_failed = YES;
      roomdCaptureFailed(self->_handle, (char *)message.UTF8String);
    });
}

- (void)forceKeyframe {
    dispatch_async(_queue, ^{
      if (self->_stopped) return;
      self->_keyframePending = YES; // the next frame is the keyframe
      if (nowNanos() - self->_lastFrameNanos < liveWindowNanos) {
          // The screen is moving: a frame is due. Re-encode only if none came.
          dispatch_after(dispatch_time(DISPATCH_TIME_NOW, liveWindowNanos), self->_queue, ^{
            [self encodePendingKeyframe];
          });
          return;
      }
      [self encodePendingKeyframe];
    });
}

- (void)setBitrate:(int)bitrate {
    dispatch_async(_queue, ^{
      if (!self->_stopped) [self applyBitrate:bitrate];
    });
}

// stop returns once no callback can reach Go any more.
- (void)stop {
    dispatch_sync(_queue, ^{
      self->_stopped = YES;
    });
    if (_stream) {
        dispatch_semaphore_t done = dispatch_semaphore_create(0);
        [_stream stopCaptureWithCompletionHandler:^(NSError *e) {
          dispatch_semaphore_signal(done);
        }];
        if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC)) != 0) {
            fprintf(stderr, "roomd-screen: stop capture: timed out; exiting stops it\n");
        }
        _stream = nil;
    }
    dispatch_sync(_queue, ^{
      if (self->_session) {
          VTCompressionSessionCompleteFrames(self->_session, kCMTimeInvalid);
          VTCompressionSessionInvalidate(self->_session);
          CFRelease(self->_session);
          self->_session = NULL;
      }
      if (self->_last) {
          CVPixelBufferRelease(self->_last);
          self->_last = NULL;
      }
    });
    if (_awake != kIOPMNullAssertionID) IOPMAssertionRelease(_awake);
    if (_activity != kIOPMNullAssertionID) IOPMAssertionRelease(_activity);
    _awake = _activity = kIOPMNullAssertionID;
}

@end

// encoded converts one encoded frame from AVCC to Annex-B, with the SPS and
// PPS ahead of a keyframe, and hands it to Go.
static void encoded(void *ref, void *frameRef, OSStatus status, VTEncodeInfoFlags flags, CMSampleBufferRef sb) {
    @autoreleasepool {
        RoomCaptureImpl *cap = (__bridge RoomCaptureImpl *)ref;
        if (status != noErr) {
            fprintf(stderr, "roomd-screen: encoder: %d\n", (int)status);
            return;
        }
        if (!sb || (flags & kVTEncodeInfo_FrameDropped) || !CMSampleBufferDataIsReady(sb)) return;
        int64_t encodeNanos = nowNanos() - (int64_t)(intptr_t)frameRef;

        bool keyframe = true;
        CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sb, false);
        if (attachments && CFArrayGetCount(attachments) > 0) {
            CFDictionaryRef a = CFArrayGetValueAtIndex(attachments, 0);
            CFBooleanRef notSync = NULL;
            if (CFDictionaryGetValueIfPresent(a, kCMSampleAttachmentKey_NotSync, (const void **)&notSync)) {
                keyframe = !CFBooleanGetValue(notSync);
            }
        }

        CMFormatDescriptionRef fmt = CMSampleBufferGetFormatDescription(sb);
        size_t sets = 0;
        int lengthSize = 4;
        if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, 0, NULL, NULL, &sets, &lengthSize) != noErr) return;
        // VideoToolbox's output is one contiguous block; copy only if not.
        CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sb);
        size_t total = CMBlockBufferGetDataLength(block), contiguous = 0;
        char *data = NULL;
        NSMutableData *copy = nil;
        if (CMBlockBufferGetDataPointer(block, 0, &contiguous, NULL, &data) != kCMBlockBufferNoErr || contiguous < total) {
            copy = [NSMutableData dataWithLength:total];
            if (CMBlockBufferCopyDataBytes(block, 0, total, copy.mutableBytes) != kCMBlockBufferNoErr) return;
            data = copy.mutableBytes;
        }

        NSMutableData *out = [NSMutableData dataWithCapacity:total + 64];
        if (keyframe) {
            for (size_t i = 0; i < sets; i++) {
                const uint8_t *set = NULL;
                size_t n = 0;
                if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, i, &set, &n, NULL, NULL) == noErr) {
                    [out appendBytes:startCode length:sizeof startCode];
                    [out appendBytes:set length:n];
                }
            }
        }
        const uint8_t *b = (const uint8_t *)data;
        for (size_t off = 0; off + (size_t)lengthSize <= total;) {
            uint32_t n = 0;
            for (int i = 0; i < lengthSize; i++) n = (n << 8) | b[off + i];
            off += (size_t)lengthSize;
            if (n == 0 || n > total - off) break;
            [out appendBytes:startCode length:sizeof startCode];
            [out appendBytes:b + off length:n];
            off += n;
        }

        CMTime pts = CMTimeConvertScale(CMSampleBufferGetPresentationTimeStamp(sb), 1000000000,
                                        kCMTimeRoundingMethod_Default);
        roomdCaptureFrame(cap->_handle, out.mutableBytes, (int)out.length, pts.value, keyframe, encodeNanos);
    }
}

void roomdMainDisplayPixels(int *width, int *height) {
    CGDirectDisplayID main = CGMainDisplayID();
    CGDisplayModeRef mode = CGDisplayCopyDisplayMode(main);
    if (mode) {
        *width = (int)CGDisplayModeGetPixelWidth(mode);
        *height = (int)CGDisplayModeGetPixelHeight(mode);
        CGDisplayModeRelease(mode);
    } else {
        *width = (int)CGDisplayPixelsWide(main);
        *height = (int)CGDisplayPixelsHigh(main);
    }
}

RoomCapture roomdCaptureStart(uintptr_t handle, int width, int height, int bitrate, bool screen, char **err) {
    @autoreleasepool {
        RoomCaptureImpl *cap = [[RoomCaptureImpl alloc] initWithHandle:handle width:width height:height];
        NSString *failure = nil;
        if (screen) {
            [cap holdDisplayAwake];
            [cap requestContent];
        }
        failure = [cap openEncoderWithBitrate:bitrate];
        if (!failure && screen) failure = [cap startStream];
        if (failure) {
            [cap stop];
            *err = strdup(failure.UTF8String);
            return NULL;
        }
        return (RoomCapture)CFBridgingRetain(cap);
    }
}

void roomdCaptureStop(RoomCapture c) {
    @autoreleasepool {
        RoomCaptureImpl *cap = CFBridgingRelease(c);
        [cap stop];
    }
}

void roomdCaptureKeyframe(RoomCapture c) { [(__bridge RoomCaptureImpl *)c forceKeyframe]; }

void roomdCaptureSetBitrate(RoomCapture c, int bitrate) { [(__bridge RoomCaptureImpl *)c setBitrate:bitrate]; }

void roomdCapturePush(RoomCapture c, int64_t ptsNanos, uint8_t shade) {
    @autoreleasepool {
        RoomCaptureImpl *cap = (__bridge RoomCaptureImpl *)c;
        NSDictionary *attrs = @{(__bridge NSString *)kCVPixelBufferIOSurfacePropertiesKey : @{}};
        CVPixelBufferRef pb = NULL;
        if (CVPixelBufferCreate(NULL, (size_t)cap->_width, (size_t)cap->_height,
                                kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange, (__bridge CFDictionaryRef)attrs,
                                &pb) != kCVReturnSuccess) {
            return;
        }
        CVPixelBufferLockBaseAddress(pb, 0);
        for (size_t plane = 0; plane < 2; plane++) {
            memset(CVPixelBufferGetBaseAddressOfPlane(pb, plane), plane == 0 ? shade : 128,
                   CVPixelBufferGetBytesPerRowOfPlane(pb, plane) * CVPixelBufferGetHeightOfPlane(pb, plane));
        }
        CVPixelBufferUnlockBaseAddress(pb, 0);
        dispatch_sync(cap->_queue, ^{
          [cap frame:pb at:CMTimeMake(ptsNanos, 1000000000)];
        });
        CVPixelBufferRelease(pb);
    }
}
