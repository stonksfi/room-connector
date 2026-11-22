package core

import (
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
