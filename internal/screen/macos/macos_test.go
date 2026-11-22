//go:build darwin && cgo

package macos

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stonksfi/room-connector/internal/screen/session"
)

// nalTypes lists the NAL unit types of an Annex-B frame with 4-byte start codes.
func nalTypes(frame []byte) []byte {
	var types []byte
	for _, nal := range bytes.Split(frame, []byte{0, 0, 0, 1})[1:] {
		if len(nal) > 0 {
			types = append(types, nal[0]&0x1f)
		}
	}
	return types
}

// The encoder needs no permission, so this runs offline on any Mac.
func TestEncoder_AnnexBWithParameterSetsOnEveryKeyframe(t *testing.T) {
	var mu sync.Mutex
	var frames []session.Frame
	c := NewCapture()
	require.NoError(t, c.start(640, 400, false, func(f session.Frame) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}, func(error) {}))
	start := 10 * time.Second
	for i := range 6 {
		c.push(start+time.Duration(i)*33*time.Millisecond, uint8(40*i))
	}
	c.ForceKeyframe() // while the screen moves, the next live frame is the keyframe
	c.push(start+6*33*time.Millisecond, 250)
	time.Sleep(150 * time.Millisecond)
	c.ForceKeyframe() // on a still screen, the last frame is encoded again
	time.Sleep(50 * time.Millisecond)
	c.Stop()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, frames, 8, "six frames, the live keyframe, and the still screen's one")
	first := frames[0]
	assert.True(t, first.Keyframe)
	assert.Equal(t, []byte{7, 8, 5}, nalTypes(first.Data)[:3], "SPS, PPS, then the IDR slice")
	sps := first.Data[4:]
	assert.Equal(t, byte(66), sps[1], "baseline profile")
	assert.NotZero(t, sps[2]&0x40, "constrained (constraint_set1)")
	assert.Equal(t, start, first.PTS)
	assert.Positive(t, first.Encode)

	assert.False(t, frames[1].Keyframe)
	assert.NotContains(t, nalTypes(frames[1].Data), byte(7), "no SPS on a delta frame")
	assert.Equal(t, start+33*time.Millisecond, frames[1].PTS)

	live := frames[6]
	assert.True(t, live.Keyframe, "the requested keyframe rides the next live frame")
	assert.Equal(t, start+6*33*time.Millisecond, live.PTS)
	still := frames[7]
	assert.True(t, still.Keyframe)
	assert.Equal(t, byte(7), nalTypes(still.Data)[0])
	assert.Greater(t, still.PTS, live.PTS, "time only moves forward")
}

func TestWheels_ContentFollowsTheFingers(t *testing.T) {
	for _, c := range []struct {
		name           string
		dx, dy         float64 // the fingers, in points: positive right and down
		wheel1, wheel2 int32   // CG: positive wheel1 moves content down, wheel2 right
	}{
		{"fingers down, content down", 0, 120, 120, 0},
		{"fingers up, content up", 0, -120, -120, 0},
		{"fingers right, content right", 80, 0, 0, 80},
		{"fingers left, content left", -80, 0, 0, -80},
		{"diagonal, rounded to whole pixels", 2.4, -2.6, -3, 2},
	} {
		wheel1, wheel2 := wheels(c.dx, c.dy)
		assert.Equal(t, []int32{c.wheel1, c.wheel2}, []int32{wheel1, wheel2}, c.name)
	}
}

func TestUTF16Chunks_NeverSplitASurrogatePair(t *testing.T) {
	assert.Empty(t, utf16Chunks("", 20))
	chunks := utf16Chunks(strings.Repeat("a", 45), 20)
	assert.Equal(t, []int{20, 20, 5}, []int{len(chunks[0]), len(chunks[1]), len(chunks[2])})

	s := strings.Repeat("a", 19) + "😀b" // the emoji's pair straddles the 20-unit boundary
	chunks = utf16Chunks(s, 20)
	require.Len(t, chunks, 2)
	assert.Len(t, chunks[0], 19)
	var joined []uint16
	for _, c := range chunks {
		joined = append(joined, c...)
	}
	assert.Equal(t, s, string(utf16.Decode(joined)))
}
