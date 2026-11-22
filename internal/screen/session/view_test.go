package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stonksfi/room-connector/internal/core"
	"github.com/stonksfi/room-connector/internal/screen"
)

const wait = 10 * time.Second

// loopback keeps ICE on host candidates of the loopback interface, so the
// two peers connect offline and fast.
func loopback(se *webrtc.SettingEngine) {
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetInterfaceFilter(func(name string) bool { return name == "lo0" || name == "lo" })
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
}

var testOptions = options{gatherTimeout: time.Second, connectTimeout: 5 * time.Second, disconnectGrace: time.Second, settings: loopback}

type fakeSource struct {
	mu        sync.Mutex
	onFrame   func(Frame)
	onError   func(error)
	startErr  error // what Start fails with, if set
	started   chan struct{}
	stopped   bool
	keyframes int
}

func newFakeSource() *fakeSource { return &fakeSource{started: make(chan struct{})} }

func (s *fakeSource) Bounds() Rect { return Rect{W: 1440, H: 900} }
func (s *fakeSource) Start(onFrame func(Frame), onError func(error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onFrame, s.onError = onFrame, onError
	close(s.started)
	return s.startErr
}
func (s *fakeSource) Stop()          { s.mu.Lock(); s.stopped = true; s.mu.Unlock() }
func (s *fakeSource) ForceKeyframe() { s.mu.Lock(); s.keyframes++; s.mu.Unlock() }
func (s *fakeSource) SetBitrate(int) {}
func (s *fakeSource) emit(f Frame)   { s.mu.Lock(); on := s.onFrame; s.mu.Unlock(); on(f) }
func (s *fakeSource) fail(err error) { s.mu.Lock(); on := s.onError; s.mu.Unlock(); on(err) }
func (s *fakeSource) state() (stopped bool, keyframes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped, s.keyframes
}

// recorder is an Injector that writes down what it was asked to post.
type recorder struct {
	mu  sync.Mutex
	got []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, fmt.Sprintf(format, args...))
}
func (r *recorder) MouseDown(p Point, b Button, n int) { r.add("down %v,%v %s %d", p.X, p.Y, b, n) }
func (r *recorder) MouseUp(p Point, b Button, n int)   { r.add("up %v,%v %s %d", p.X, p.Y, b, n) }
func (r *recorder) MouseMove(p Point, held Button)     { r.add("move %v,%v %q", p.X, p.Y, held) }
func (r *recorder) Scroll(p Point, dx, dy float64)     { r.add("scroll %v,%v %v %v", p.X, p.Y, dx, dy) }
func (r *recorder) Text(s string)                      { r.add("text %s", s) }
func (r *recorder) Key(k SpecialKey)                   { r.add("key %s", k) }
func (r *recorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

type fakeIndicator struct {
	mu     sync.Mutex
	onStop func()
}

func (i *fakeIndicator) Show(onStop func()) { i.mu.Lock(); i.onStop = onStop; i.mu.Unlock() }
func (i *fakeIndicator) Hide()              {}
func (i *fakeIndicator) stop()              { i.mu.Lock(); f := i.onStop; i.mu.Unlock(); f() }

func granted() core.ScreenPermissions {
	return core.ScreenPermissions{ScreenRecording: true, Accessibility: true}
}

// phone is the iPhone's side: a recvonly video offer and the input channel.
type phone struct {
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	opened   chan struct{}
	messages chan string
	frames   chan received
	ssrc     chan webrtc.SSRC
}

type received struct {
	data []byte
	ts   uint32
}

func newPhone(t *testing.T) *phone {
	t.Helper()
	m := &webrtc.MediaEngine{}
	require.NoError(t, m.RegisterDefaultCodecs())
	reg := &interceptor.Registry{}
	require.NoError(t, webrtc.RegisterDefaultInterceptors(m, reg))
	se := webrtc.SettingEngine{}
	loopback(&se)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se)).
		NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() }) // test teardown
	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	require.NoError(t, err)
	dc, err := pc.CreateDataChannel(channelLabel, nil)
	require.NoError(t, err)
	ph := &phone{pc: pc, dc: dc, opened: make(chan struct{}), messages: make(chan string, 16),
		frames: make(chan received, 64), ssrc: make(chan webrtc.SSRC, 1)}
	dc.OnOpen(func() { close(ph.opened) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) { ph.messages <- string(m.Data) })
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		ph.ssrc <- tr.SSRC()
		go ph.readFrames(tr)
	})
	return ph
}

// readFrames reassembles each frame's Annex-B from its RTP packets.
func (ph *phone) readFrames(tr *webrtc.TrackRemote) {
	var depack codecs.H264Packet
	var frame []byte
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		nal, err := depack.Unmarshal(pkt.Payload)
		if err != nil {
			continue
		}
		frame = append(frame, nal...)
		if pkt.Marker {
			select {
			case ph.frames <- received{data: frame, ts: pkt.Timestamp}:
			default:
			}
			frame = nil
		}
	}
}

func (ph *phone) offer(t *testing.T) string {
	t.Helper()
	offer, err := ph.pc.CreateOffer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(ph.pc)
	require.NoError(t, ph.pc.SetLocalDescription(offer))
	<-gathered
	return ph.pc.LocalDescription().SDP
}

func (ph *phone) send(t *testing.T, msg string) {
	t.Helper()
	require.NoError(t, ph.dc.SendText(msg))
}

func (ph *phone) next(t *testing.T) string {
	t.Helper()
	select {
	case m := <-ph.messages:
		return m
	case <-time.After(wait):
		t.Fatal("no message from the Mac")
		return ""
	}
}

// live is a view served by Serve, as roomd-screen runs it, with the phone
// connected through the wire format.
type live struct {
	phone  *phone
	src    *fakeSource
	inj    *recorder
	ind    *fakeIndicator
	stdin  io.WriteCloser
	result chan Reason
}

// startLive is a live view: connected, capturing, the display size sent.
func startLive(t *testing.T) *live {
	t.Helper()
	l := handshake(t, newFakeSource())
	select {
	case <-l.src.started:
	case <-time.After(wait):
		t.Fatal("capture never started")
	}
	select {
	case <-l.phone.opened:
	case <-time.After(wait):
		t.Fatal("the input channel never opened")
	}
	assert.JSONEq(t, `{"t":"display","w":1440,"h":900}`, l.phone.next(t))
	return l
}

// handshake serves a view of src and has the phone apply its answer.
func handshake(t *testing.T, src *fakeSource) *live {
	t.Helper()
	l := &live{phone: newPhone(t), src: src, inj: &recorder{}, ind: &fakeIndicator{}, result: make(chan Reason, 1)}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	l.stdin = inW
	t.Cleanup(func() { inW.Close() })
	p := Platform{Source: l.src, Injector: l.inj, Indicator: l.ind, Permissions: granted}
	go func() {
		reason, err := serve(context.Background(), inR, outW, p, testOptions)
		assert.NoError(t, err)
		l.result <- reason
	}()

	req, err := json.Marshal(screen.Request{Offer: core.ScreenOffer{ID: "so_1", SDP: l.phone.offer(t)}})
	require.NoError(t, err)
	_, err = inW.Write(append(req, '\n'))
	require.NoError(t, err)
	line, err := bufio.NewReader(outR).ReadBytes('\n')
	require.NoError(t, err)
	var rep screen.Reply
	require.NoError(t, json.Unmarshal(line, &rep))
	require.Empty(t, rep.Unavailable)
	require.Contains(t, rep.SDP, "a=candidate", "non-trickle: the answer carries its candidates")
	require.Contains(t, rep.SDP, "H264")
	require.NoError(t, l.phone.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rep.SDP}))
	return l
}

func (l *live) ended(t *testing.T) Reason {
	t.Helper()
	select {
	case r := <-l.result:
		return r
	case <-time.After(wait):
		t.Fatal("the view did not end")
		return ""
	}
}

func annexB(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

func TestView_StreamsFramesAndInjectsInput(t *testing.T) {
	l := startLive(t)

	// A keyframe big enough to be fragmented, then a frame 33ms later.
	idr := append([]byte{0x65}, bytes.Repeat([]byte{0xab}, 3000)...)
	key := Frame{Data: annexB([]byte{0x67, 0x42, 0xe0, 0x1f, 0x01}, []byte{0x68, 0xce, 0x3c}, idr), PTS: time.Second, Keyframe: true}
	next := Frame{Data: annexB([]byte{0x41, 0x9a, 0x02}), PTS: time.Second + 33*time.Millisecond}
	var first received
	deadline := time.After(wait)
	for first.data == nil { // the phone's receiver may miss the first packets
		l.src.emit(key)
		select {
		case first = <-l.phone.frames:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("no frame reached the phone")
		}
	}
	assert.Equal(t, key.Data, first.data, "the keyframe arrives whole, SPS and PPS included")
	l.src.emit(next)
	var second received
	for second.data == nil || bytes.Equal(second.data, key.Data) {
		second = <-l.phone.frames
	}
	assert.Equal(t, next.Data, second.data)
	assert.Equal(t, uint32(2970), second.ts-first.ts, "RTP time follows the capture time at 90 kHz")

	ssrc := <-l.phone.ssrc
	require.NoError(t, l.phone.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}}))
	assert.Eventually(t, func() bool { _, n := l.src.state(); return n > 0 }, wait, 10*time.Millisecond, "a PLI forces a keyframe")

	l.phone.send(t, `{"t":"down","x":0.5,"y":0.5,"b":"left"}`)
	l.phone.send(t, `{"t":"up","x":0.5,"y":0.5,"b":"left"}`)
	l.phone.send(t, `{"t":"down","x":0.25,"y":0.25,"b":"left"}`)
	l.phone.send(t, `{"t":"move","x":0.75,"y":0.5}`)
	l.phone.send(t, `{"t":"scroll","x":0.5,"y":0.5,"dx":0.1,"dy":-0.2}`)
	l.phone.send(t, `{"t":"nope"}`)
	l.phone.send(t, `{"t":"text","s":"hi"}`)
	l.phone.send(t, `{"t":"key","k":"return"}`)
	l.phone.send(t, `{"t":"bye"}`)

	assert.Equal(t, Closed, l.ended(t))
	assert.Equal(t, []string{
		"down 720,450 left 1",
		"up 720,450 left 1",
		"down 360,225 left 1",
		`move 1080,450 "left"`,
		"scroll 720,450 144 -180",
		"text hi",
		"key return",
		"up 1080,450 left 1", // the view ended with the left button still held
	}, l.inj.events())
	stopped, _ := l.src.state()
	assert.True(t, stopped, "capture stops with the view")
}

func TestView_StopViewingSaysByeToThePhone(t *testing.T) {
	l := startLive(t)
	l.ind.stop()
	assert.JSONEq(t, `{"t":"bye","reason":"stopped_on_mac"}`, l.phone.next(t))
	assert.Equal(t, StoppedOnMac, l.ended(t))
}

func TestView_CaptureFailingSaysByeToThePhone(t *testing.T) {
	l := startLive(t)
	l.src.fail(errors.New("the display went away"))
	assert.JSONEq(t, `{"t":"bye","reason":"capture_failed"}`, l.phone.next(t))
	assert.Equal(t, CaptureFailed, l.ended(t))
}

func TestView_CaptureFailingToStartSaysByeToThePhone(t *testing.T) {
	src := newFakeSource()
	src.startErr = errors.New("the person declined Screen Recording")
	l := handshake(t, src)
	msg := l.phone.next(t)
	if strings.Contains(msg, `"display"`) { // sent if the channel opened before capture was tried
		msg = l.phone.next(t)
	}
	assert.JSONEq(t, `{"t":"bye","reason":"capture_failed"}`, msg)
	assert.Equal(t, CaptureFailed, l.ended(t))
}

func TestView_RoomdGoingAwayEndsTheView(t *testing.T) {
	l := startLive(t)
	require.NoError(t, l.stdin.Close())
	assert.Equal(t, StoppedOnMac, l.ended(t))
}

func TestView_NeverAnsweredEndsLost(t *testing.T) {
	ph := newPhone(t)
	src := newFakeSource()
	opts := testOptions
	opts.connectTimeout = 500 * time.Millisecond
	v, sdp, err := answer(context.Background(), core.ScreenOffer{ID: "so_1", SDP: ph.offer(t)},
		Platform{Source: src, Injector: &recorder{}, Indicator: &fakeIndicator{}, Permissions: granted}, opts)
	require.NoError(t, err)
	require.NotEmpty(t, sdp)
	// The phone never applies the answer.
	assert.Equal(t, Lost, v.Run(context.Background()))
	select {
	case <-src.started:
		t.Fatal("capture started without a connection")
	default:
	}
}

func TestView_InputWaitsForThePicture(t *testing.T) {
	ph := newPhone(t)
	inj := &recorder{}
	v, _, err := answer(context.Background(), core.ScreenOffer{ID: "so_1", SDP: ph.offer(t)},
		Platform{Source: newFakeSource(), Injector: inj, Indicator: &fakeIndicator{}, Permissions: granted}, testOptions)
	require.NoError(t, err)
	v.inputs <- []byte(`{"t":"down","x":0.5,"y":0.5,"b":"left"}`) // before the peer connected
	v.inputs <- []byte(`{"t":"bye"}`)
	assert.Equal(t, Closed, v.Run(context.Background()))
	assert.Empty(t, inj.events(), "nothing is posted before capture and the indicator are up")
}

func TestServe_ErrorsNeverQuoteTheOffer(t *testing.T) {
	var lines []string
	for _, l := range strings.Split(newPhone(t).offer(t), "\r\n") {
		if strings.HasPrefix(l, "a=candidate:") {
			l = "a=candidate:1 1 udp 2122260223 198.51.100.7 99999 typ host"
		}
		lines = append(lines, l)
	}
	req, err := json.Marshal(screen.Request{Offer: core.ScreenOffer{ID: "so_1", SDP: strings.Join(lines, "\r\n")}})
	require.NoError(t, err)
	var out bytes.Buffer
	p := Platform{Source: newFakeSource(), Injector: &recorder{}, Indicator: &fakeIndicator{}, Permissions: granted}
	_, err = serve(context.Background(), bytes.NewReader(append(req, '\n')), &out, p, testOptions)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "198.51.100.7", "the phone's address never reaches the log")
	assert.JSONEq(t, `{"unavailable":"failed"}`, out.String())
}

func TestServe_ReportsWhatIsMissing(t *testing.T) {
	req := `{"offer":{"id":"so_1","sdp":"v=0"}}` + "\n"
	for perms, want := range map[core.ScreenPermissions]screen.Unavailable{
		{Accessibility: true}:   screen.NeedsScreenRecording,
		{}:                      screen.NeedsScreenRecording,
		{ScreenRecording: true}: screen.NeedsAccessibility,
	} {
		var out bytes.Buffer
		p := Platform{Source: newFakeSource(), Injector: &recorder{}, Indicator: &fakeIndicator{}, Permissions: func() core.ScreenPermissions { return perms }}
		_, err := serve(context.Background(), bytes.NewBufferString(req), &out, p, testOptions)
		require.NoError(t, err)
		assert.JSONEq(t, `{"unavailable":"`+string(want)+`"}`, out.String())
	}

	var out bytes.Buffer
	p := Platform{Source: newFakeSource(), Injector: &recorder{}, Indicator: &fakeIndicator{}, Permissions: granted}
	_, err := serve(context.Background(), bytes.NewBufferString(`{"offer":{"id":"so_1","sdp":"not sdp"}}`+"\n"), &out, p, testOptions)
	assert.Error(t, err)
	assert.JSONEq(t, `{"unavailable":"failed"}`, out.String())
}
