package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stonksfi/room-connector/internal/core"
)

func TestDescribePermissions_NamesWhereToGrantWhatIsMissing(t *testing.T) {
	assert.Contains(t, describePermissions(core.ScreenPermissions{ScreenRecording: true, Accessibility: true}), "Ready")

	both := describePermissions(core.ScreenPermissions{})
	assert.Contains(t, both, "Screen & System Audio Recording")
	assert.Contains(t, both, "Accessibility")

	one := describePermissions(core.ScreenPermissions{ScreenRecording: true})
	assert.NotContains(t, one, "Screen & System Audio Recording")
	assert.Contains(t, one, "Privacy & Security > Accessibility")
}
