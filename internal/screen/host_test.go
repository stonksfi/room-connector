package screen

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stonksfi/room-connector/internal/core"
)

// fakeEnv makes the test binary act as roomd-screen: Host runs it in its place.
const fakeEnv = "FAKE_ROOMD_SCREEN"

func TestMain(m *testing.M) {
	if behaviour := os.Getenv(fakeEnv); behaviour != "" {
		os.Exit(fakeRoomdScreen(behaviour))
	}
	os.Exit(m.Run())
}

// fakeRoomdScreen speaks the wire protocol the way roomd-screen does.
func fakeRoomdScreen(behaviour string) int {
	if os.Args[1] == "setup" {
		fmt.Println(`{"screen_recording":true,"accessibility":false}`)
		return 0
	}
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return 1
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return 1
	}
	switch behaviour {
	case "needs_accessibility":
		fmt.Println(`{"unavailable":"needs_accessibility"}`)
		return 0
	case "mute":
		return 1
	}
	fmt.Fprintln(os.Stderr, "serving", req.Offer, strings.Repeat("x", 2*maxLogLine))
	fmt.Printf(`{"sdp":"answer to %s from %d"}`+"\n", req.Offer.ID, os.Getpid())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	go func() { // roomd went away
		_, _ = io.Copy(io.Discard, in) // the fake ends either way
		os.Exit(0)
	}()
	<-stop
	return 0
}

func testHost(t *testing.T, behaviour string) *Host {
	t.Helper()
	t.Setenv(fakeEnv, behaviour)
	self, err := os.Executable()
	require.NoError(t, err)
	h := NewHost(self)
	t.Cleanup(h.Stop)
	return h
}

func pidOf(t *testing.T, sdp string) int {
	t.Helper()
	var id string
	var pid int
	_, err := fmt.Sscanf(sdp, "answer to %s from %d", &id, &pid)
	require.NoError(t, err, sdp)
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestHost_AnswerReplaceStop(t *testing.T) {
	h := testHost(t, "answer")
	ctx := context.Background()

	sdp, err := h.Answer(ctx, core.ScreenOffer{ID: "so_1", SDP: "offer"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sdp, "answer to so_1 "), sdp)
	first := pidOf(t, sdp)
	assert.True(t, alive(first), "the child keeps serving the view")

	sdp, err = h.Answer(ctx, core.ScreenOffer{ID: "so_2", SDP: "offer"})
	require.NoError(t, err)
	second := pidOf(t, sdp)
	assert.False(t, alive(first), "a new view replaces the old one")
	assert.True(t, alive(second))

	h.Stop()
	assert.False(t, alive(second), "Stop leaves no child")
	h.Stop() // nothing to stop
}

func TestHost_ConcurrentAnswersLeaveOneChild(t *testing.T) {
	h := testHost(t, "answer")
	sdps := make(chan string, 2)
	for _, id := range []string{"so_1", "so_2"} {
		go func() {
			sdp, _ := h.Answer(context.Background(), core.ScreenOffer{ID: id, SDP: "offer"}) // the loser's error is expected
			sdps <- sdp
		}()
	}
	live := 0
	for range 2 {
		if sdp := <-sdps; sdp != "" && alive(pidOf(t, sdp)) {
			live++
		}
	}
	assert.Equal(t, 1, live, "one view wins; the other is gone")
}

func TestHost_ASupersededAnswerNeverDisplacesTheView(t *testing.T) {
	h := testHost(t, "answer")
	sdp, err := h.Answer(context.Background(), core.ScreenOffer{ID: "so_2", SDP: "offer"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a newer offer already ended this one's view
	_, err = h.Answer(ctx, core.ScreenOffer{ID: "so_1", SDP: "offer"})
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, alive(pidOf(t, sdp)), "the newer view runs on")
}

func TestHost_ContextEndsTheView(t *testing.T) {
	h := testHost(t, "answer")
	ctx, cancel := context.WithCancel(context.Background())
	sdp, err := h.Answer(ctx, core.ScreenOffer{ID: "so_1", SDP: "offer"})
	require.NoError(t, err)
	pid := pidOf(t, sdp)
	cancel()
	assert.Eventually(t, func() bool { return !alive(pid) }, 5*time.Second, 10*time.Millisecond)
}

func TestHost_MapsWhatTheChildCannotDo(t *testing.T) {
	_, err := testHost(t, "needs_accessibility").Answer(context.Background(), core.ScreenOffer{ID: "so_1"})
	assert.ErrorIs(t, err, core.ErrNeedsAccessibility)

	_, err = testHost(t, "mute").Answer(context.Background(), core.ScreenOffer{ID: "so_1"})
	assert.ErrorContains(t, err, "exited without answering")

	_, err = NewHost("/nonexistent/roomd-screen").Answer(context.Background(), core.ScreenOffer{ID: "so_1"})
	assert.Error(t, err)
}

func TestHost_Setup(t *testing.T) {
	p, err := testHost(t, "answer").Setup(context.Background())
	require.NoError(t, err)
	assert.Equal(t, core.ScreenPermissions{ScreenRecording: true}, p)
}

func TestLineWriter_SplitsAndBoundsLines(t *testing.T) {
	var lines []string
	w := &lineWriter{max: 5, line: func(l string) { lines = append(lines, l) }}
	for _, chunk := range []string{"ab", "c\nde", "fghij\n\nxy"} {
		n, err := w.Write([]byte(chunk))
		require.NoError(t, err)
		assert.Equal(t, len(chunk), n)
	}
	assert.Equal(t, []string{"abc", "defgh", ""}, lines)
}

func TestScreenOffer_NeverPrintsItsSecrets(t *testing.T) {
	o := core.ScreenOffer{ID: "so_1", SDP: "c=IN IP4 192.0.2.1", ICEServers: []core.ICEServer{{Credential: "secret"}}}
	for _, s := range []string{fmt.Sprint(o), fmt.Sprintf("%v %+v", o, o), fmt.Sprint(o.ICEServers)} {
		assert.NotContains(t, s, "192.0.2.1")
		assert.NotContains(t, s, "secret")
	}
}
