package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"
)

// The `input` data channel carries JSON text messages of at most maxMessage
// bytes. x,y are fractions of the video frame; dx,dy are fractions of its size.
const (
	maxMessage = 4 << 10
	maxText    = 256 // characters in one text message
)

// Point is a position: a fraction of the frame, or global display points.
type Point struct{ X, Y float64 }

// Rect is the captured display in global points.
type Rect struct{ X, Y, W, H float64 }

// Button is a mouse button, as the wire names it. The zero value is none.
type Button string

const (
	Left  Button = "left"
	Right Button = "right"
)

// SpecialKey is a key the phone's keyboard sends by name, not as text.
type SpecialKey string

const (
	Backspace SpecialKey = "backspace"
	Return    SpecialKey = "return"
	// ShiftTab is Tab with Shift held (back-tab).
	ShiftTab SpecialKey = "shift_tab"
)

// Input is one message from the phone: Down, Move, Up, Scroll, Text, Key or Bye.
type Input interface{ input() }

type (
	Down struct {
		At     Point
		Button Button
	}
	Move struct{ At Point }
	Up   struct {
		At     Point
		Button Button
	}
	Scroll struct {
		At     Point
		DX, DY float64
	}
	Text struct{ S string }
	Key  struct{ K SpecialKey }
	Bye  struct{}
)

func (Down) input()   {}
func (Move) input()   {}
func (Up) input()     {}
func (Scroll) input() {}
func (Text) input()   {}
func (Key) input()    {}
func (Bye) input()    {}

// ErrTooLarge is a message over the channel's size limit.
var ErrTooLarge = errors.New("input message too large")

// message is every field any input message carries.
type message struct {
	T  string  `json:"t"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	B  string  `json:"b"`
	DX float64 `json:"dx"`
	DY float64 `json:"dy"`
	S  string  `json:"s"`
	K  string  `json:"k"`
}

// Parse decodes one channel message. Its errors name the message type at
// most, never what was typed, so they are safe to log.
func Parse(raw []byte) (Input, error) {
	if len(raw) > maxMessage {
		return nil, ErrTooLarge
	}
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("input message is not JSON")
	}
	at := Point{m.X, m.Y}
	switch m.T {
	case "down", "up":
		b, err := parseButton(m.B)
		if err != nil {
			return nil, err
		}
		if m.T == "down" {
			return Down{At: at, Button: b}, nil
		}
		return Up{At: at, Button: b}, nil
	case "move":
		return Move{At: at}, nil
	case "scroll":
		return Scroll{At: at, DX: m.DX, DY: m.DY}, nil
	case "text":
		if utf8.RuneCountInString(m.S) > maxText {
			return nil, ErrTooLarge
		}
		return Text{S: m.S}, nil
	case "key":
		switch k := SpecialKey(m.K); k {
		case Backspace, Return, ShiftTab:
			return Key{K: k}, nil
		}
		return nil, errors.New("key message names an unknown key")
	case "bye":
		return Bye{}, nil
	}
	return nil, fmt.Errorf("unknown input message type %.16q", m.T)
}

func parseButton(b string) (Button, error) {
	switch Button(b) {
	case "", Left:
		return Left, nil
	case Right:
		return Right, nil
	}
	return "", errors.New("mouse message names an unknown button")
}

// Map converts a point given as fractions of the frame to global points.
func Map(norm Point, bounds Rect) Point {
	return Point{
		X: bounds.X + clamp01(norm.X)*bounds.W,
		Y: bounds.Y + clamp01(norm.Y)*bounds.H,
	}
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// Click counting follows macOS: a press soon after and near the last one, with
// the same button, continues a double (or triple...) click.
const (
	clickInterval = 500 * time.Millisecond
	clickSlop     = 6.0 // points
)

// ClickCounter numbers each press for kCGMouseEventClickState.
type ClickCounter struct {
	at     time.Time
	where  Point
	button Button
	count  int
}

// Down records a press and returns its click count.
func (c *ClickCounter) Down(now time.Time, p Point, b Button) int {
	if c.count > 0 && b == c.button && now.Sub(c.at) <= clickInterval &&
		math.Hypot(p.X-c.where.X, p.Y-c.where.Y) <= clickSlop {
		c.count++
	} else {
		c.count = 1
	}
	c.at, c.where, c.button = now, p, b
	return c.count
}

// Count is the click count of the last press, which its release repeats.
func (c *ClickCounter) Count() int { return max(c.count, 1) }
