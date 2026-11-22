// Package session is one screen view as roomd-screen serves it: the WebRTC
// peer that answers the phone's offer, the Mac's view state machine, and the
// `input` data channel. It is pure Go; the screen, the encoder, input
// injection and the menu bar indicator are the Platform's (internal/screen/macos).
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/stonksfi/room-connector/internal/core"
)

// Frame is one encoded H.264 access unit in Annex-B, with SPS and PPS ahead
// of every keyframe.
type Frame struct {
	Data []byte
	// PTS is the capture time on a clock that only moves forward.
	PTS      time.Duration
	Keyframe bool
	// Encode is how long the encoder took (bench reports it).
	Encode time.Duration
}

// Source captures the display and encodes it.
type Source interface {
	// Bounds is the captured display in global points.
	Bounds() Rect
	// Start captures until Stop, calling onFrame for each frame and onError
	// once if capture stops by itself. Both may run on any goroutine, and
	// neither runs after Stop returns.
	Start(onFrame func(Frame), onError func(error)) error
	Stop()
	// ForceKeyframe makes the next frame a keyframe, even on a still screen.
	ForceKeyframe()
	// SetBitrate sets the encoder's target in bits per second, before or
	// after Start.
	SetBitrate(bps int)
}

// FitLongSide scales a w×h capture down so its long side is at most limit,
// keeping the aspect ratio and both sides even, as 4:2:0 video needs.
func FitLongSide(w, h, limit int) (int, int) {
	if long := max(w, h); long > limit {
		w = int(math.Round(float64(w) * float64(limit) / float64(long)))
		h = int(math.Round(float64(h) * float64(limit) / float64(long)))
	}
	return w &^ 1, h &^ 1
}

// Injector posts input to the system, at global points.
type Injector interface {
	MouseDown(p Point, b Button, clicks int)
	MouseUp(p Point, b Button, clicks int)
	// MouseMove moves the pointer, dragging with held unless it is "".
	MouseMove(p Point, held Button)
	// Scroll scrolls at p by dx,dy points; content follows the fingers.
	Scroll(p Point, dx, dy float64)
	Text(s string)
	Key(k SpecialKey)
}

// Indicator shows the person at the Mac that their screen is being viewed.
type Indicator interface {
	// Show displays it; onStop runs when they choose Stop viewing.
	Show(onStop func())
	Hide()
}

// Platform is what a view needs from the operating system.
type Platform struct {
	Source    Source
	Injector  Injector
	Indicator Indicator
	// Permissions reports which permissions a view needs are granted.
	Permissions func() core.ScreenPermissions
}

// Reason is why a view ended.
type Reason string

const (
	Closed        Reason = "closed"         // the phone said bye
	Lost          Reason = "lost"           // the connection failed or never came up
	StoppedOnMac  Reason = "stopped_on_mac" // Stop viewing, `roomd screen stop`, roomd went away
	CaptureFailed Reason = "capture_failed"
)

// options is a view's timing; tests shorten it.
type options struct {
	gatherTimeout   time.Duration // non-trickle ICE: send what was gathered by then
	connectTimeout  time.Duration // from the answer to connected
	disconnectGrace time.Duration // how long `disconnected` may last
	// settings adjusts pion's setting engine; tests keep ICE on loopback.
	settings func(*webrtc.SettingEngine)
}

var defaults = options{gatherTimeout: 2 * time.Second, connectTimeout: 15 * time.Second, disconnectGrace: 10 * time.Second}

// The encoder starts at startBitrate; congestion control (GCC) moves it
// between minBitrate and maxBitrate.
const (
	startBitrate = 2_500_000
	minBitrate   = 300_000
	maxBitrate   = 8_000_000
	bitratePoll  = time.Second

	channelLabel = "input"
	byeFlush     = 500 * time.Millisecond
	// mtu is pion's outbound RTP packet size.
	mtu = 1200
	// h264 is constrained baseline, which every phone decodes in hardware.
	h264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
)

// View is one screen view: answered, then served by Run.
type View struct {
	pc     *webrtc.PeerConnection
	track  *webrtc.TrackLocalStaticRTP
	bwe    cc.BandwidthEstimator
	p      Platform
	opts   options
	bounds Rect

	// Pion callbacks feed the Run loop through these.
	states     chan webrtc.PeerConnectionState
	opened     chan *webrtc.DataChannel
	inputs     chan []byte
	keyframes  chan struct{}
	captureErr chan error
	stop       chan struct{}
	stopOnce   sync.Once
	done       chan struct{} // closed when the view ends; callbacks stop blocking

	// Owned by the Run loop.
	dc        *webrtc.DataChannel
	capturing bool
	held      map[Button]bool
	pointer   Point
	clicks    ClickCounter
	bitrate   int

	// Frames arrive on the encoder's goroutine, one at a time.
	frameMu    sync.Mutex
	packetizer rtp.Packetizer
	tsBase     uint32
	firstPTS   time.Duration
	timed      bool
}

// answer builds the peer for offer and returns the view and its answer SDP:
// complete, with the candidates gathered within gatherTimeout. The caller
// must Run the view, which releases it.
func answer(ctx context.Context, offer core.ScreenOffer, p Platform, opts options) (*View, string, error) {
	v := &View{
		p:          p,
		opts:       opts,
		bounds:     p.Source.Bounds(),
		states:     make(chan webrtc.PeerConnectionState, 8),
		opened:     make(chan *webrtc.DataChannel, 1),
		inputs:     make(chan []byte, 64),
		keyframes:  make(chan struct{}, 1),
		captureErr: make(chan error, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		held:       map[Button]bool{},
		packetizer: rtp.NewPacketizer(mtu, 0, 0, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000),
		tsBase:     rand.Uint32(),
	}
	if err := v.connect(ctx, offer); err != nil {
		if v.pc != nil {
			close(v.done)
			if cerr := v.pc.Close(); cerr != nil {
				log.Printf("close peer: %v", cerr)
			}
		}
		return nil, "", err
	}
	return v, v.pc.LocalDescription().SDP, nil
}

func (v *View) connect(ctx context.Context, offer core.ScreenOffer) error {
	pc, bwe, err := newPeer(offer.ICEServers, v.opts)
	if err != nil {
		return fmt.Errorf("create peer: %w", err)
	}
	v.pc, v.bwe = pc, bwe
	v.track, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp,
	}, "screen", "room")
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(v.track)
	if err != nil {
		return err
	}
	go v.readRTCP(sender)
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { deliver(v, v.states, s) })
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != channelLabel {
			return
		}
		dc.OnOpen(func() { deliver(v, v.opened, dc) })
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				deliver(v, v.inputs, m.Data)
			}
		})
	})

	// pion's errors here can quote the SDP, addresses included, and every
	// error reaches roomd's log: report only which step failed.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return errors.New("the offer is not a usable WebRTC offer")
	}
	if len(sender.GetParameters().Codecs) == 0 {
		return errors.New("the offer has no H.264 video to receive")
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return errors.New("could not create the answer")
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return errors.New("could not apply the answer")
	}
	gather := time.NewTimer(v.opts.gatherTimeout)
	defer gather.Stop()
	select {
	case <-gathered:
	case <-gather.C: // answer with what was gathered: signaling is one round trip
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// newPeer makes a peer that sends H.264 under send-side congestion control.
func newPeer(servers []core.ICEServer, opts options) (*webrtc.PeerConnection, cc.BandwidthEstimator, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp,
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, nil, err
	}
	reg := &interceptor.Registry{}
	congestion, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(startBitrate),
			gcc.SendSideBWEMinBitrate(minBitrate),
			gcc.SendSideBWEMaxBitrate(maxBitrate),
		)
	})
	if err != nil {
		return nil, nil, err
	}
	var bwe cc.BandwidthEstimator
	// Called synchronously while NewPeerConnection builds the interceptors.
	congestion.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { bwe = e })
	reg.Add(congestion)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, reg); err != nil {
		return nil, nil, err
	}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, nil, err
	}
	se := webrtc.SettingEngine{}
	// The connection's loggers (ICE, DTLS, TURN...) can print addresses, and
	// SDP at the levels PION_LOG_* turns on, and roomd's log takes our
	// stderr: silence them. The interceptors log RTP statistics only.
	se.LoggerFactory = &logging.DefaultLoggerFactory{Writer: io.Discard, DefaultLogLevel: logging.LogLevelDisabled}
	if opts.settings != nil {
		opts.settings(&se)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers(servers)})
	if err != nil {
		return nil, nil, err
	}
	return pc, bwe, nil
}

func iceServers(in []core.ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(in))
	for _, s := range in {
		out = append(out, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	return out
}

// deliver hands a pion callback's value to the Run loop, giving up once the
// view has ended so pion's goroutines never block on a loop that is gone.
func deliver[T any](v *View, ch chan T, x T) {
	select {
	case ch <- x:
	case <-v.done:
	}
}

// readRTCP answers the phone's keyframe requests. Reading RTCP is also what
// feeds NACK and congestion control.
func (v *View) readRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return // the peer closed
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				select {
				case v.keyframes <- struct{}{}:
				default: // one is already pending
				}
			}
		}
	}
}

// Stop ends the view as stopped on the Mac. Safe from any goroutine.
func (v *View) Stop() {
	v.stopOnce.Do(func() { close(v.stop) })
}

// Run serves the view until it ends, then releases everything it holds.
func (v *View) Run(ctx context.Context) Reason {
	reason := v.loop(ctx)
	v.end(reason)
	return reason
}

func (v *View) loop(ctx context.Context) Reason {
	connect := time.NewTimer(v.opts.connectTimeout)
	defer connect.Stop()
	var lost <-chan time.Time
	poll := time.NewTicker(bitratePoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return StoppedOnMac
		case <-v.stop:
			return StoppedOnMac
		case <-connect.C:
			log.Printf("view: no connection within %s", v.opts.connectTimeout)
			return Lost
		case <-lost:
			log.Printf("view: disconnected for %s", v.opts.disconnectGrace)
			return Lost
		case s := <-v.states:
			switch s {
			case webrtc.PeerConnectionStateConnected:
				lost = nil
				if !v.capturing {
					connect.Stop()
					if err := v.startCapture(); err != nil {
						log.Printf("view: start capture: %v", err)
						return CaptureFailed
					}
				}
			case webrtc.PeerConnectionStateDisconnected:
				if lost == nil {
					lost = time.After(v.opts.disconnectGrace)
				}
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				log.Printf("view: connection %s", s)
				return Lost
			}
		case dc := <-v.opened:
			// The display's size, once, as the channel opens: the phone shapes
			// the picture by it until the first frame is decoded.
			v.dc = dc
			v.send(map[string]any{"t": "display", "w": v.bounds.W, "h": v.bounds.H})
		case raw := <-v.inputs:
			in, err := Parse(raw)
			if err != nil {
				log.Printf("view: drop input: %v", err)
				continue
			}
			if _, bye := in.(Bye); bye {
				return Closed
			}
			if !v.capturing {
				continue // dropped: no input before the picture, and the indicator, are up
			}
			v.apply(in, time.Now())
		case <-v.keyframes:
			if v.capturing {
				v.p.Source.ForceKeyframe()
			}
		case err := <-v.captureErr:
			log.Printf("view: capture stopped: %v", err)
			return CaptureFailed
		case <-poll.C:
			v.pollBitrate()
		}
	}
}

func (v *View) startCapture() error {
	v.bitrate = startBitrate
	v.p.Source.SetBitrate(v.bitrate)
	err := v.p.Source.Start(v.writeFrame, func(err error) {
		select {
		case v.captureErr <- err:
		default: // the first error ends the view
		}
	})
	if err != nil {
		return err
	}
	v.capturing = true
	v.p.Indicator.Show(v.Stop)
	return nil
}

// pollBitrate follows congestion control's estimate with the encoder.
func (v *View) pollBitrate() {
	if !v.capturing || v.bwe == nil {
		return
	}
	if bps := v.bwe.GetTargetBitrate(); bps != v.bitrate {
		v.bitrate = bps
		v.p.Source.SetBitrate(bps)
	}
}

// writeFrame sends one encoded frame, stamped with its capture time so
// playout keeps the screen's own timing even when idle frames are skipped.
func (v *View) writeFrame(f Frame) {
	v.frameMu.Lock()
	if !v.timed {
		v.firstPTS, v.timed = f.PTS, true
	}
	ts := v.tsBase + uint32(int64(f.PTS-v.firstPTS)*9/100_000) // 90 kHz
	pkts := v.packetizer.Packetize(f.Data, 0)
	v.frameMu.Unlock()
	for _, pkt := range pkts {
		pkt.Timestamp = ts
		// A packet that cannot be sent is dropped: NACK, keyframe requests
		// and the connection state handle the rest.
		_ = v.track.WriteRTP(pkt)
	}
}

func (v *View) send(msg any) {
	raw, err := json.Marshal(msg)
	if err == nil {
		err = v.dc.SendText(string(raw))
	}
	if err != nil {
		log.Printf("view: send to the phone: %v", err)
	}
}

func (v *View) apply(in Input, now time.Time) {
	inj := v.p.Injector
	switch m := in.(type) {
	case Down:
		if v.held[m.Button] {
			return
		}
		v.pointer = Map(m.At, v.bounds)
		v.held[m.Button] = true
		inj.MouseDown(v.pointer, m.Button, v.clicks.Down(now, v.pointer, m.Button))
	case Up:
		if !v.held[m.Button] {
			return
		}
		v.pointer = Map(m.At, v.bounds)
		delete(v.held, m.Button)
		inj.MouseUp(v.pointer, m.Button, v.clicks.Count())
	case Move:
		v.pointer = Map(m.At, v.bounds)
		inj.MouseMove(v.pointer, v.dragging())
	case Scroll:
		inj.Scroll(Map(m.At, v.bounds), m.DX*v.bounds.W, m.DY*v.bounds.H)
	case Text:
		inj.Text(m.S)
	case Key:
		inj.Key(m.K)
	}
}

func (v *View) dragging() Button {
	for _, b := range []Button{Left, Right} {
		if v.held[b] {
			return b
		}
	}
	return ""
}

// end releases the view in order: held buttons, capture, a bye if the Mac
// ended it, then the peer.
func (v *View) end(reason Reason) {
	for _, b := range []Button{Left, Right} {
		if v.held[b] {
			v.p.Injector.MouseUp(v.pointer, b, v.clicks.Count())
			delete(v.held, b)
		}
	}
	if v.capturing {
		v.p.Source.Stop()
		v.capturing = false
		v.p.Indicator.Hide()
	}
	if reason == StoppedOnMac || reason == CaptureFailed {
		if v.dc == nil {
			// The channel may be opening just as the view ends (capture can
			// fail as it starts): wait a moment, so the phone hears why
			// rather than seeing the view lost.
			select {
			case v.dc = <-v.opened:
			case <-time.After(byeFlush):
			}
		}
		if v.dc != nil {
			v.send(map[string]string{"t": "bye", "reason": string(reason)})
			v.flushChannel()
		}
	}
	close(v.done)
	if err := v.pc.GracefulClose(); err != nil {
		log.Printf("view: close peer: %v", err)
	}
}

// flushChannel gives a last message up to byeFlush to reach the phone before
// the peer closes.
func (v *View) flushChannel() {
	deadline := time.NewTimer(byeFlush)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for v.dc.BufferedAmount() > 0 {
		select {
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}
