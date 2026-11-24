package core

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestTruncate(t *testing.T) {
	assert.Equal(t, "short", Truncate("short", 10))
	got := Truncate("héllo wörld", 8)
	assert.LessOrEqual(t, len(got), 8)
	assert.True(t, utf8.ValidString(got), "never splits a character")
	assert.Equal(t, "h…", Truncate("héllo", 5), "backs off to a character boundary")
}

func TestAsk_FitsRoom(t *testing.T) {
	q := func(text string, labels ...string) AskQuestion {
		out := AskQuestion{Question: text}
		for _, l := range labels {
			out.Options = append(out.Options, AskOption{Label: l})
		}
		return out
	}
	long := func(n int) string { return strings.Repeat("a", n) }
	assert.True(t, (&Ask{Kind: AskKindPlan}).FitsRoom())
	assert.True(t, (&Ask{Kind: AskKindQuestion, Questions: []AskQuestion{q("A?", "x", "y")}}).FitsRoom())
	for name, a := range map[string]*Ask{
		"a plan with questions": {Kind: AskKindPlan, Questions: []AskQuestion{q("A?", "x")}},
		"no questions":          {Kind: AskKindQuestion},
		"an unknown kind":       {Kind: "poll", Questions: []AskQuestion{q("A?", "x")}},
		"an empty question":     {Kind: AskKindQuestion, Questions: []AskQuestion{q("", "x")}},
		"a long question":       {Kind: AskKindQuestion, Questions: []AskQuestion{q(long(MaxAskQuestion+1), "x")}},
		"a repeated question":   {Kind: AskKindQuestion, Questions: []AskQuestion{q("A?", "x"), q("A?", "y")}},
		"no options":            {Kind: AskKindQuestion, Questions: []AskQuestion{q("A?")}},
		"a repeated label":      {Kind: AskKindQuestion, Questions: []AskQuestion{q("A?", "x", "x")}},
		"an empty label":        {Kind: AskKindQuestion, Questions: []AskQuestion{q("A?", "")}},
		"a long label":          {Kind: AskKindQuestion, Questions: []AskQuestion{q("A?", long(MaxAskLabel+1))}},
		"too many questions":    {Kind: AskKindQuestion, Questions: slices.Repeat([]AskQuestion{q("A?", "x")}, MaxAskQuestions+1)},
		"a long header":         {Kind: AskKindQuestion, Questions: []AskQuestion{{Question: "A?", Header: long(MaxAskHeader + 1), Options: []AskOption{{Label: "x"}}}}},
		"a long description":    {Kind: AskKindQuestion, Questions: []AskQuestion{{Question: "A?", Options: []AskOption{{Label: "x", Description: long(MaxAskDescription + 1)}}}}},
	} {
		assert.False(t, a.FitsRoom(), name)
	}
}
