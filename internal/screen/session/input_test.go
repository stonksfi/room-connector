package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_EveryMessage(t *testing.T) {
	cases := map[string]Input{
		`{"t":"down","x":0.25,"y":0.5,"b":"left"}`:         Down{At: Point{0.25, 0.5}, Button: Left},
		`{"t":"down","x":0.1,"y":0.2,"b":"right"}`:         Down{At: Point{0.1, 0.2}, Button: Right},
		`{"t":"move","x":0.3,"y":0.4}`:                     Move{At: Point{0.3, 0.4}},
		`{"t":"up","x":0.25,"y":0.5,"b":"left"}`:           Up{At: Point{0.25, 0.5}, Button: Left},
		`{"t":"scroll","x":0.5,"y":0.5,"dx":0,"dy":-0.1}`:  Scroll{At: Point{0.5, 0.5}, DY: -0.1},
		`{"t":"text","s":"héllo ✓"}`:                       Text{S: "héllo ✓"},
		`{"t":"key","k":"backspace"}`:                      Key{K: Backspace},
		`{"t":"key","k":"return"}`:                         Key{K: Return},
		`{"t":"key","k":"shift_tab"}`:                      Key{K: ShiftTab},
		`{"t":"bye"}`:                                      Bye{},
		`{"t":"down","x":0.5,"y":0.5,"future_field":true}`: Down{At: Point{0.5, 0.5}, Button: Left},
	}
	for raw, want := range cases {
		got, err := Parse([]byte(raw))
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}
}

func TestParse_DropsWhatItCannotUse(t *testing.T) {
	for _, raw := range []string{
		`{"t":"paste","s":"secret"}`,
		`{"t":"key","k":"escape"}`,
		`{"t":"down","x":0.5,"y":0.5,"b":"middle"}`,
		`not json`,
		`{"t":"text","s":"` + strings.Repeat("a", maxText+1) + `"}`,
		`{"t":"move","x":0.5,"y":0.5,"pad":"` + strings.Repeat("x", maxMessage) + `"}`,
	} {
		_, err := Parse([]byte(raw))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret", "an error never repeats what was sent")
	}
	_, err := Parse([]byte(`{"t":"text","s":"` + strings.Repeat("é", maxText) + `"}`))
	assert.NoError(t, err, "the text limit counts characters, not bytes")
}

func TestMap_ToGlobalPoints(t *testing.T) {
	bounds := Rect{X: 0, Y: 0, W: 1512, H: 982}
	assert.Equal(t, Point{756, 491}, Map(Point{0.5, 0.5}, bounds))
	assert.Equal(t, Point{0, 982}, Map(Point{-0.2, 1.5}, bounds), "clamped to the display")
	assert.Equal(t, Point{-1440 + 360, 225}, Map(Point{0.25, 0.25}, Rect{X: -1440, Y: 0, W: 1440, H: 900}))
}

func TestClickCounter(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	var c ClickCounter
	assert.Equal(t, 1, c.Down(t0, Point{100, 100}, Left))
	assert.Equal(t, 2, c.Down(t0.Add(300*time.Millisecond), Point{103, 100}, Left), "300ms and 3pt apart: a double click")
	assert.Equal(t, 2, c.Count(), "the release repeats the press's count")
	assert.Equal(t, 3, c.Down(t0.Add(600*time.Millisecond), Point{103, 100}, Left))
	assert.Equal(t, 1, c.Down(t0.Add(1300*time.Millisecond), Point{103, 100}, Left), "700ms later: a new click")
	assert.Equal(t, 1, c.Down(t0.Add(1400*time.Millisecond), Point{120, 100}, Left), "too far away")
	assert.Equal(t, 1, c.Down(t0.Add(1500*time.Millisecond), Point{120, 100}, Right), "another button")
}

func TestFitLongSide(t *testing.T) {
	cases := []struct{ w, h, wantW, wantH int }{
		{3024, 1964, 2560, 1662},
		{1964, 3024, 1662, 2560},
		{1920, 1080, 1920, 1080},
		{5120, 2880, 2560, 1440},
		{1513, 983, 1512, 982},
	}
	for _, c := range cases {
		w, h := FitLongSide(c.w, c.h, 2560)
		assert.Equal(t, []int{c.wantW, c.wantH}, []int{w, h}, "%dx%d", c.w, c.h)
	}
}
