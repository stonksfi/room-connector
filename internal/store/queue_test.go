package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stonksfi/room-connector/internal/core"
)

func TestQueue_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q", "events.jsonl")
	q, err := OpenQueue(path)
	require.NoError(t, err)
	require.NoError(t, q.Push(core.Event{ClientEventID: "a"}, core.Event{ClientEventID: "b"}, core.Event{ClientEventID: "c"}))
	require.NoError(t, q.Drop(q.Peek(1)[0].Seq))
	require.NoError(t, q.Close())

	reopened, err := OpenQueue(path)
	require.NoError(t, err)
	assert.Equal(t, 2, reopened.Len())
	got := reopened.Peek(10)
	assert.Equal(t, "b", got[0].Event.ClientEventID)
	assert.Equal(t, "c", got[1].Event.ClientEventID)

	// New events continue the sequence after a reopen.
	require.NoError(t, reopened.Push(core.Event{ClientEventID: "d"}))
	assert.Greater(t, reopened.Peek(10)[2].Seq, got[1].Seq)
}

func TestQueue_DropBySeqKeepsEventsTrimmedMeanwhile(t *testing.T) {
	q, err := OpenQueue(filepath.Join(t.TempDir(), "events.jsonl"))
	require.NoError(t, err)
	require.NoError(t, q.Push(core.Event{ClientEventID: "a"}, core.Event{ClientEventID: "b"}))
	batch := q.Peek(1) // "a" is being sent
	require.NoError(t, q.Drop(batch[0].Seq))
	require.NoError(t, q.Drop(batch[0].Seq)) // a repeat drops nothing more
	assert.Equal(t, "b", q.Peek(1)[0].Event.ClientEventID)
}

func TestQueue_CompactsAndSkipsTornLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	q, err := OpenQueue(path)
	require.NoError(t, err)
	for i := 0; i < compactAfter+10; i++ {
		require.NoError(t, q.Push(core.Event{ClientEventID: "e"}))
	}
	require.NoError(t, q.Drop(q.Peek(compactAfter)[compactAfter-1].Seq))
	assert.Equal(t, 10, q.Len())
	require.NoError(t, q.Close())

	// A crash mid-append leaves a torn last line.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"seq":99999,"event":{"Clie`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	reopened, err := OpenQueue(path)
	require.NoError(t, err)
	assert.Equal(t, 10, reopened.Len())
}

func TestQueue_DropsOldestPastLimit(t *testing.T) {
	q, err := OpenQueue(filepath.Join(t.TempDir(), "events.jsonl"))
	require.NoError(t, err)
	batch := make([]core.Event, MaxQueued+3)
	for i := range batch {
		batch[i] = core.Event{ClientEventID: string(rune('a' + i%26))}
	}
	require.NoError(t, q.Push(batch...))
	assert.Equal(t, MaxQueued, q.Len())
	assert.Equal(t, batch[3].ClientEventID, q.Peek(1)[0].Event.ClientEventID)
}
