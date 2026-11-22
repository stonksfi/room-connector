package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stonksfi/room-connector/internal/core"
)

// recorder is a core.Sink that keeps what it was told.
type recorder struct {
	mu        sync.Mutex
	upserts   []core.Session
	events    []core.Event
	snapshots [][]core.Session
	settled   map[int64]error
}

func newRecorder() *recorder { return &recorder{settled: map[int64]error{}} }

func (r *recorder) UpsertSession(s core.Session) {
	r.mu.Lock()
	r.upserts = append(r.upserts, s)
	r.mu.Unlock()
}
func (r *recorder) Emit(e core.Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }
func (r *recorder) Snapshot(_ string, live []core.Session) {
	r.mu.Lock()
	r.snapshots = append(r.snapshots, live)
	r.mu.Unlock()
}
func (r *recorder) Settle(id int64, err error) { r.mu.Lock(); r.settled[id] = err; r.mu.Unlock() }

func (r *recorder) kinds() []core.EventKind {
	out := make([]core.EventKind, len(r.events))
	for i, e := range r.events {
		out[i] = e.Kind
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestProvider(t *testing.T) (*Provider, *recorder, *clock) {
	t.Helper()
	p := New("")
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	p.now = c.now
	r := newRecorder()
	p.sink = r
	return p, r, c
}

func hook(t *testing.T, p *Provider, fields map[string]any) []byte {
	t.Helper()
	if _, ok := fields["session_id"]; !ok {
		fields["session_id"] = "s1"
	}
	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	out, err := p.HandleHook(context.Background(), raw)
	require.NoError(t, err)
	return out
}

func TestHooks_TurnBecomesConversationAndStates(t *testing.T) {
	p, r, _ := newTestProvider(t)

	hook(t, p, map[string]any{"hook_event_name": "SessionStart", "cwd": "/work/room"})
	require.Len(t, r.upserts, 1)
	assert.Equal(t, "room", r.upserts[0].Title, "titled after its folder")
	assert.Equal(t, core.StateSleeping, r.upserts[0].State)

	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "fix the bug"})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "t1",
		"tool_input": map[string]any{"command": "go   test\n ./..."}})
	// Chunks out of order: the reply is reported once all are in.
	hook(t, p, map[string]any{"hook_event_name": "MessageDisplay", "message_id": "m1", "index": 1, "delta": "world", "final": true})
	hook(t, p, map[string]any{"hook_event_name": "MessageDisplay", "message_id": "m1", "index": 0, "delta": "hello "})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "hello world"})

	assert.Equal(t, []core.EventKind{
		core.KindUserMessage, core.KindStateChange, core.KindToolActivity, core.KindAssistantMessage, core.KindStateChange,
	}, r.kinds())
	assert.Equal(t, "fix the bug", r.events[0].Text)
	assert.Equal(t, core.StateWorking, r.events[1].State)
	assert.Equal(t, "go test ./...", r.events[2].Text, "tool input collapsed to one line")
	assert.Equal(t, "hello world", r.events[3].Text)
	assert.Equal(t, core.StateSleeping, r.events[4].State)
	for _, e := range r.events {
		assert.Equal(t, Name, e.Provider)
		assert.Equal(t, "s1", e.ProviderSessionID)
		assert.NotEmpty(t, e.ClientEventID)
	}
}

func TestHooks_StopReportsReplyWhenNoChunksArrived(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "hi"})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "hey"})
	require.Len(t, r.events, 4)
	assert.Equal(t, core.KindAssistantMessage, r.events[2].Kind)
	assert.Equal(t, "hey", r.events[2].Text)
}

func assistantTexts(r *recorder) []string {
	texts := []string{}
	for _, e := range r.events {
		if e.Kind == core.KindAssistantMessage {
			texts = append(texts, e.ClientEventID+"="+e.Text)
		}
	}
	return texts
}

func TestHooks_StopCompletesDraftAndIgnoresLateChunks(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "hi"})
	hook(t, p, map[string]any{"hook_event_name": "MessageDisplay", "message_id": "m1", "index": 0, "delta": "line one\n"})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "line one\nline two"})
	// The trailing chunk arrives after Stop (hooks run in the background).
	hook(t, p, map[string]any{"hook_event_name": "MessageDisplay", "message_id": "m1", "index": 1, "delta": "line two", "final": true})
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p2", "prompt": "again"})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "second reply"})
	assert.Equal(t, []string{"msg:m1=line one\nline two", "stop:p2:1=second reply"}, assistantTexts(r))
}

func TestHooks_LateChunksAfterStopReplyAreNotDuplicated(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "hi"})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Done."})
	// Every chunk of the final reply arrives after its Stop.
	hook(t, p, map[string]any{"hook_event_name": "MessageDisplay", "message_id": "m1", "index": 0, "delta": "Done.", "final": true})
	assert.Equal(t, []string{"stop:p1:1=Done."}, assistantTexts(r))
}

func TestHooks_CompactionStartKeepsWorking(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "long"})
	hook(t, p, map[string]any{"hook_event_name": "SessionStart", "source": "compact"})
	assert.Equal(t, core.StateWorking, r.upserts[len(r.upserts)-1].State)
}

func TestHooks_SessionEndEndsAndFailsUndelivered(t *testing.T) {
	p, r, _ := newTestProvider(t)
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 7, Text: "hello"}))
	hook(t, p, map[string]any{"hook_event_name": "SessionEnd"})
	require.Len(t, r.events, 1)
	assert.Equal(t, core.StateEnded, r.events[0].State)
	assert.ErrorIs(t, r.settled[7], errEnded)
	assert.NotContains(t, p.sessions, "s1")
}

func TestHooks_IgnoresInputWithoutSessionOrBeforeStart(t *testing.T) {
	p, r, _ := newTestProvider(t)
	out, err := p.HandleHook(context.Background(), []byte(`{"hook_event_name":"Stop"}`))
	require.NoError(t, err)
	assert.Nil(t, out)
	_, err = p.HandleHook(context.Background(), []byte(`not json`))
	assert.Error(t, err)

	p.sink = nil
	_, err = p.HandleHook(context.Background(), []byte(`{"hook_event_name":"Stop","session_id":"s1"}`))
	require.NoError(t, err)
	assert.Empty(t, r.events)
}

func TestInbox_MessageIsDeliveredWhenItShowsUpAsPrompt(t *testing.T) {
	p, r, _ := newTestProvider(t)
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 3, Text: "run the tests"}))

	msgs := p.TakeInbox("s1")
	require.Equal(t, []core.InboxMessage{{CommandID: 3, Text: "run the tests"}}, msgs)
	assert.Empty(t, p.TakeInbox("s1"), "a message is handed out once")
	_, settled := r.settled[3]
	assert.False(t, settled, "not delivered until it runs")

	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p9", "prompt": "run the tests\n"})
	assert.NoError(t, r.settled[3])
	assert.Equal(t, int64(3), r.events[0].CommandID)
}

func TestStopFallback_HandsMessageWhenNoScriptPolls(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "hi"})
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 5, Text: "also update docs"}))
	c.advance(pollFresh + time.Second)

	out := hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "done"})
	var block stopBlock
	require.NoError(t, json.Unmarshal(out, &block))
	assert.Equal(t, "block", block.Decision)
	assert.Contains(t, block.Reason, "also update docs")
	last := r.events[len(r.events)-1]
	assert.Equal(t, core.KindUserMessage, last.Kind, "reported as the person's message; the turn continues")
	assert.Equal(t, int64(5), last.CommandID)
	assert.NotContains(t, r.settled, int64(5), "not delivered until Claude is seen continuing")

	// Claude continues and stops again: the message landed, and the
	// continuation's reply has an id of its own.
	hook(t, p, map[string]any{"hook_event_name": "Stop", "stop_hook_active": true, "last_assistant_message": "docs updated"})
	assert.NoError(t, r.settled[5])
	assert.Equal(t, []string{"stop:p1:1=done", "stop:p1:2=docs updated"}, assistantTexts(r))
}

func TestStopFailure_NeverHandsOverAMessage(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "hi"})
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 5, Text: "x"}))
	c.advance(pollFresh + time.Second)
	out := hook(t, p, map[string]any{"hook_event_name": "StopFailure"})
	assert.Nil(t, out, "a background hook's answer is ignored")
	assert.NotContains(t, r.settled, int64(5))
	assert.Len(t, p.sessions["s1"].inbox, 1, "still queued for the next Stop")
	assert.Equal(t, core.StateSleeping, r.events[len(r.events)-1].State)
}

func TestStopFallback_SkippedWhenTheHookGaveUp(t *testing.T) {
	p, _, c := newTestProvider(t)
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 5, Text: "x"}))
	c.advance(pollFresh + time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := p.HandleHook(ctx, []byte(`{"hook_event_name":"Stop","session_id":"s1"}`))
	require.NoError(t, err)
	assert.Nil(t, out)
	assert.Len(t, p.sessions["s1"].inbox, 1)
}

func TestReturnInbox_FailsWithTheScriptsReason(t *testing.T) {
	p, r, _ := newTestProvider(t)
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 3, Text: "x"}))
	p.TakeInbox("s1")
	p.ReturnInbox("s1", 99, "unknown") // not handed: ignored
	p.ReturnInbox("nope", 3, "unknown")
	assert.Empty(t, r.settled)
	p.ReturnInbox("s1", 3, "blocked by a hook")
	require.Error(t, r.settled[3])
	assert.Contains(t, r.settled[3].Error(), "blocked by a hook")
	assert.Empty(t, p.sessions["s1"].handed)
}

func TestStopFallback_NotUsedWhileScriptPolls(t *testing.T) {
	p, _, _ := newTestProvider(t)
	p.TakeInbox("s1") // the script is alive
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 5, Text: "x"}))
	out := hook(t, p, map[string]any{"hook_event_name": "Stop"})
	assert.Nil(t, out)
}

func TestExpire_FailsUntakenAndUnconfirmed(t *testing.T) {
	p, r, c := newTestProvider(t)
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 1, Text: "a"}))
	require.NoError(t, p.Deliver(context.Background(), "s2", core.Command{ID: 2, Text: "b"}))
	p.TakeInbox("s2") // taken, never run

	c.advance(untakenTimeout + time.Second)
	p.expire()
	assert.ErrorIs(t, r.settled[1], errUntaken)
	assert.ErrorIs(t, r.settled[2], errUnconfirmed)
}

func TestExpire_KeepsUntakenMessageWhileSessionWorks(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p", "prompt": "long task"})
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 1, Text: "a"}))
	c.advance(untakenTimeout * 3)
	p.expire()
	assert.NotContains(t, r.settled, int64(1), "the turn's Stop can still hand it over")
}

func TestExpire_KeepsTakenMessageWhileSessionWorks(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1", "prompt_id": "p", "prompt": "long task"})
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 1, Text: "a"}))
	p.TakeInbox("s1")
	c.advance(unconfirmedTimeout * 3)
	p.expire()
	assert.NotContains(t, r.settled, int64(1), "a long turn delays it legitimately")
}

func TestReconcile_SnapshotsLiveAndForgetsGone(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "SessionStart", "session_id": "gone"})
	require.NoError(t, p.Deliver(context.Background(), "gone", core.Command{ID: 9, Text: "x"}))
	c.advance(hookFresh + time.Second)

	p.listLive = func(context.Context) ([]liveSession, error) {
		return []liveSession{{SessionID: "live", Cwd: "/w/app", Name: "my-task", Status: "busy", StartedAt: 1_700_000_000_000}}, nil
	}
	p.reconcile(context.Background())

	require.Len(t, r.snapshots, 1)
	require.Len(t, r.snapshots[0], 1)
	got := r.snapshots[0][0]
	assert.Equal(t, "live", got.ProviderSessionID)
	assert.Equal(t, "my-task", got.Title)
	assert.Equal(t, core.StateWorking, got.State)
	assert.Equal(t, time.UnixMilli(1_700_000_000_000), got.StartedAt)
	assert.ErrorIs(t, r.settled[9], errEnded, "a vanished session's messages fail")
}

func TestReconcile_FreshHooksWin(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1", "prompt_id": "p", "prompt": "go"})
	hook(t, p, map[string]any{"hook_event_name": "SessionStart", "session_id": "new"})
	p.listLive = func(context.Context) ([]liveSession, error) {
		return []liveSession{{SessionID: "s1", Status: "idle"}}, nil
	}
	p.reconcile(context.Background())
	states := map[string]core.State{}
	for _, s := range r.snapshots[0] {
		states[s.ProviderSessionID] = s.State
	}
	assert.Equal(t, core.StateWorking, states["s1"], "the hook just said it is working")
	assert.Contains(t, states, "new", "a session whose hooks are ahead of `claude agents` is kept")
}

func TestReconcile_SkipsWhenListingFails(t *testing.T) {
	p, r, _ := newTestProvider(t)
	p.listLive = func(context.Context) ([]liveSession, error) { return nil, errors.New("no claude") }
	p.reconcile(context.Background())
	assert.Empty(t, r.snapshots)
}

func TestSummarizeTool(t *testing.T) {
	long := make([]byte, maxToolSummary+50)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"Read", map[string]any{"file_path": "/a/b.go"}, "/a/b.go"},
		{"Grep", map[string]any{"pattern": "TODO"}, "TODO"},
		{"WebFetch", map[string]any{"url": "https://x"}, "https://x"},
		{"WebSearch", map[string]any{"query": "go"}, "go"},
		{"Agent", map[string]any{"description": "explore"}, "explore"},
		{"mcp__x", map[string]any{"query": "q"}, "q"},
		{"Bash", map[string]any{"command": string(long)}, string(long[:maxToolSummary-len("…")]) + "…"},
		{"Bash", map[string]any{}, ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, summarizeTool(tc.tool, tc.input), tc.tool)
	}
}

func TestStart_ReconcilesUntilCancelled(t *testing.T) {
	p := New("")
	r := newRecorder()
	called := make(chan struct{}, 1)
	p.listLive = func(context.Context) ([]liveSession, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- p.Start(ctx, r) }()
	<-called
	cancel()
	assert.NoError(t, <-done)
}
