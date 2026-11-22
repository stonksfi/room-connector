package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandLog_SurvivesRestartWithoutReRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commands.json")
	l, err := OpenCommandLog(path)
	require.NoError(t, err)

	first, err := l.MarkDispatched(1)
	require.NoError(t, err)
	assert.True(t, first)
	again, err := l.MarkDispatched(1)
	require.NoError(t, err)
	assert.False(t, again, "handed out once")

	_, err = l.MarkDispatched(2)
	require.NoError(t, err)
	require.NoError(t, l.AddSettle(PendingSettle{CommandID: 2}))

	restarted, err := OpenCommandLog(path)
	require.NoError(t, err)
	assert.True(t, restarted.Inherited(1), "handed out, fate unknown: not re-run")
	assert.False(t, restarted.Inherited(2), "its outcome is known and still owed")
	assert.Equal(t, []PendingSettle{{CommandID: 2}}, restarted.Settles())
	dup, err := restarted.MarkDispatched(1)
	require.NoError(t, err)
	assert.False(t, dup)
}

func TestCommandLog_AckedIsForgottenLater(t *testing.T) {
	l, err := OpenCommandLog(filepath.Join(t.TempDir(), "commands.json"))
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	require.NoError(t, l.AddSettle(PendingSettle{CommandID: 1, Failure: "x"}))
	require.NoError(t, l.Acked(1, now))
	assert.Empty(t, l.Settles())
	again, err := l.MarkDispatched(1)
	require.NoError(t, err)
	assert.False(t, again, "remembered for a while after its ack")

	require.NoError(t, l.Acked(2, now.Add(ackedRemembered+time.Minute)))
	fresh, err := l.MarkDispatched(1)
	require.NoError(t, err)
	assert.True(t, fresh, "pruned after ackedRemembered")
}
