package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func TestHooks_HarnessPromptIsANoteThatStartsATurn(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": handback})

	assert.Equal(t, []core.EventKind{core.KindNote, core.KindStateChange}, r.kinds())
	assert.Equal(t, "Subagent report", r.events[0].Label)
	assert.True(t, strings.HasPrefix(r.events[0].Text, "## Code review"), r.events[0].Text)
	assert.Equal(t, "prompt:p1", r.events[0].ClientEventID)
	assert.Equal(t, core.StateWorking, r.events[1].State, "the agent goes on to act on it")
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

func TestInbox_AppMessageThatLooksLikeAHarnessTagStaysThePersons(t *testing.T) {
	p, r, _ := newTestProvider(t)
	text := "<agent-message>hi</agent-message>"
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 4, Text: text}))
	p.TakeInbox("s1")

	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": text})
	assert.Equal(t, core.KindUserMessage, r.events[0].Kind)
	assert.Equal(t, int64(4), r.events[0].CommandID)
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

// stateChanges lists the state changes reported, as "id=state".
func stateChanges(r *recorder) []string {
	out := []string{}
	for _, e := range r.events {
		if e.Kind == core.KindStateChange {
			out = append(out, e.ClientEventID+"="+string(e.State))
		}
	}
	return out
}

var rmBuild = map[string]any{"command": "rm -rf build"}

func TestWait_PermissionEndsAtThePermittedToolsPostToolUse(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_use_id": "t0", "tool_input": map[string]any{"file_path": "/a"}})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "t1", "tool_input": rmBuild})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)

	// A parallel call finishing is not the answer.
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "tool_use_id": "t0", "tool_input": map[string]any{"file_path": "/a"}})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)

	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "t1", "tool_input": rmBuild})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func TestWait_NamesTheToolItAsksAbout(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_use_id": "t9", "tool_input": map[string]any{"file_path": "/a"}})
	hook(t, p, map[string]any{"hook_event_name": "Elicitation", "mcp_server_name": "github"})

	named := []string{}
	for _, e := range r.events {
		if e.Kind == core.KindStateChange {
			named = append(named, string(e.State)+":"+e.ToolName)
		}
	}
	// The wait names its tool; going back to work and an elicitation name none
	assert.Equal(t, []string{"working:", "needs_input:Bash", "working:", "needs_input:"}, named)
}

func TestWait_DeniedToolEndsWhenClaudeMovesOn(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "t1", "tool_input": rmBuild})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	// Denied: no hook. Claude goes on with another call.
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "t2", "tool_input": map[string]any{"command": "ls build"}})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func TestWait_LatePreToolUseOfTheWaitedToolDoesNotEndIt(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	// Both run in the background: the prompt's hook can overtake the call's.
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "t1", "tool_input": rmBuild})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input"}, stateChanges(r))
}

func TestWait_QuestionEndsAtItsAnswer(t *testing.T) {
	p, r, _ := newTestProvider(t)
	question := map[string]any{"questions": []any{map[string]any{"question": "Which db?"}}}
	answered := map[string]any{"questions": question["questions"], "answers": map[string]any{"Which db?": "Postgres"}}
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "set up the db"})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "q1", "tool_input": question})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "AskUserQuestion", "tool_input": question})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)
	// The answer comes back in the call's input, so it is matched by its id.
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "q1", "tool_input": answered})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

// askID is the id a question with this tool and input is reported under in turn p1.
func askID(tool string, input map[string]any) string {
	return "ask:p1:" + printFor(tool, input)
}

func TestAsk_ShownOnceAsTheAgentsMessageBeforeItsWait(t *testing.T) {
	cases := []struct {
		name, tool string
		input      map[string]any
		order      []string // the hooks of the one call, in arrival order
		want       string
	}{
		{"question", "AskUserQuestion", asks(question("Which db?", false, option("Postgres", "Relational"))),
			[]string{"PreToolUse", "PermissionRequest"}, "Which db?\n\n- **Postgres** — Relational"},
		{"plan whose permission overtakes", "ExitPlanMode", map[string]any{"plan": "# Plan\n\n1. Do it"},
			[]string{"PermissionRequest", "PreToolUse"}, "# Plan\n\n1. Do it"},
	}
	for _, tc := range cases {
		p, r, _ := newTestProvider(t)
		hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
		for _, name := range tc.order {
			hook(t, p, map[string]any{"hook_event_name": name, "tool_name": tc.tool, "tool_use_id": "t1", "tool_input": tc.input})
		}
		// No tool row: the question, once, then the wait that names it.
		assert.Equal(t, []core.EventKind{core.KindUserMessage, core.KindStateChange, core.KindAssistantMessage, core.KindStateChange}, r.kinds(), tc.name)
		assert.Equal(t, []string{askID(tc.tool, tc.input) + "=" + tc.want}, assistantTexts(r), tc.name)
		assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input"}, stateChanges(r), tc.name)
		assert.Equal(t, tc.tool, r.events[3].ToolName, tc.name)
	}
}

func TestAsk_TheTurnsReplyIsStillReportedAtStop(t *testing.T) {
	p, r, _ := newTestProvider(t)
	plan := map[string]any{"plan": "# Plan"}
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "plan it"})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "ExitPlanMode", "tool_use_id": "x1", "tool_input": plan})
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "tool_use_id": "x1", "tool_input": plan})
	// No chunks arrived: the Stop's own text is the reply.
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Done."})
	assert.Equal(t, []string{askID("ExitPlanMode", plan) + "=# Plan", "stop:p1:1=Done."}, assistantTexts(r))
}

func TestAsk_TheSameQuestionInALaterTurnShowsAgain(t *testing.T) {
	p, r, _ := newTestProvider(t)
	q := asks(question("Ship it?", false))
	for _, prompt := range []string{"p1", "p2"} {
		hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": prompt, "prompt": "go"})
		hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "q" + prompt, "tool_input": q})
	}
	first := askID("AskUserQuestion", q)
	assert.Equal(t, []string{first + "=Ship it?", strings.Replace(first, "p1", "p2", 1) + "=Ship it?"}, assistantTexts(r))
}

func TestWait_TheNewestPromptIsTheOneOpen(t *testing.T) {
	p, r, _ := newTestProvider(t)
	ls := map[string]any{"command": "ls build"}
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	// Granted; a parallel call's prompt opens before the first call ends.
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": ls})
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "t1", "tool_input": rmBuild})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "t2", "tool_input": ls})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func TestWait_ElicitationEndsAtItsResult(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "deploy"})
	hook(t, p, map[string]any{"hook_event_name": "Elicitation", "mcp_server_name": "deployer", "message": "Which region?"})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)
	hook(t, p, map[string]any{"hook_event_name": "ElicitationResult", "mcp_server_name": "deployer", "action": "accept"})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func TestWait_NewReplyEndsItAndLateChunksDoNot(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	hook(t, p, map[string]any{"hook_event_name": "MessageDisplay", "message_id": "m1", "index": 0, "delta": "Let me "})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	hook(t, p, map[string]any{"hook_event_name": "MessageDisplay", "message_id": "m1", "index": 1, "delta": "clean up.", "final": true})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State, "a late chunk of a reply begun before the prompt")
	hook(t, p, map[string]any{"hook_event_name": "MessageDisplay", "message_id": "m2", "index": 0, "delta": "OK, I won't."})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func TestWait_NotificationOnlyWhenItNeedsThePerson(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	hook(t, p, map[string]any{"hook_event_name": "Notification", "notification_type": "idle_prompt"})
	hook(t, p, map[string]any{"hook_event_name": "Notification", "notification_type": "auth_success"})
	assert.Equal(t, core.StateWorking, p.sessions["s1"].info.State)

	hook(t, p, map[string]any{"hook_event_name": "Notification", "notification_type": "permission_prompt"})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)
	// The prompt's own hook arriving after its notification changes nothing.
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input"}, stateChanges(r))
}

func TestWait_StopWhileWaitingSleepsAndLateHooksAreIgnored(t *testing.T) {
	p, r, _ := newTestProvider(t)
	// Between turns nothing is waiting on the person.
	hook(t, p, map[string]any{"hook_event_name": "Notification", "notification_type": "agent_needs_input"})
	assert.Empty(t, stateChanges(r))

	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Stopped."})
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "t1", "tool_input": rmBuild})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_use_id": "t2", "tool_input": map[string]any{}})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "sleep:p1:1=sleeping"}, stateChanges(r))
}

func TestWait_ADialogBetweenTurnsIsNotEndedByALateHook(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Done."})
	p.listLive = func(context.Context) ([]liveSession, error) {
		return []liveSession{{SessionID: "s1", Status: "waiting", WaitingFor: "dialog open"}}, nil
	}
	c.advance(hookFresh + time.Second)
	p.reconcile(context.Background())
	require.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)

	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_use_id": "t9", "tool_input": map[string]any{}})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State, "a late hook of the finished turn")
	assert.Equal(t, []string{"work:p1=working", "sleep:p1:1=sleeping"}, stateChanges(r))
}

func TestWait_EveryStateChangeOfATurnHasItsOwnID(t *testing.T) {
	p, r, _ := newTestProvider(t)
	for _, prompt := range []string{"p1", "p2"} {
		hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": prompt, "prompt": "go"})
		for i, tool := range []string{"a", "b", "c"} {
			input := map[string]any{"command": tool}
			id := prompt + tool
			hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": id, "tool_input": input})
			hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": input})
			if i == 1 {
				hook(t, p, map[string]any{"hook_event_name": "Elicitation"})
				hook(t, p, map[string]any{"hook_event_name": "ElicitationResult"})
				continue
			}
			hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": id, "tool_input": input})
		}
		hook(t, p, map[string]any{"hook_event_name": "Stop"})
	}
	changes := stateChanges(r)
	require.Len(t, changes, 2*(2+3*2))
	assert.Contains(t, changes, "wait:p2:1=needs_input", "numbered per turn")
	ids := map[string]bool{}
	for _, c := range changes {
		id, _, _ := strings.Cut(c, "=")
		assert.False(t, ids[id], "repeated id %s", id)
		ids[id] = true
	}
}

func TestWait_StopThatHandsOverAMessageGoesOnWorking(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
	hook(t, p, map[string]any{"hook_event_name": "Notification", "notification_type": "agent_needs_input"})
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 5, Text: "also update docs"}))
	c.advance(pollFresh + time.Second)
	require.NotNil(t, hook(t, p, map[string]any{"hook_event_name": "Stop"}))
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func tasksRunning(types ...string) []any {
	tasks := make([]any, len(types))
	for i, typ := range types {
		tasks[i] = map[string]any{"type": typ}
	}
	return tasks
}

func TestStop_AgentsStillRunningKeepTheSessionWorking(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "review the diff"})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Two reviewers are running.",
		"background_tasks": tasksRunning("subagent", "shell")})
	assert.Equal(t, []string{"work:p1=working"}, stateChanges(r), "not done while its agents run")
	assert.Equal(t, core.StateWorking, p.sessions["s1"].info.State)
	assert.Equal(t, []string{"stop:p1:1=Two reviewers are running."}, assistantTexts(r), "the turn's reply is still reported")

	// The agent's report starts the next turn, which ends with nothing running.
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p2", "prompt": handback})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Both found nothing.", "background_tasks": []any{}})
	assert.Equal(t, []string{"work:p1=working", "work:p2=working", "sleep:p2:1=sleeping"}, stateChanges(r))
}

func TestStop_OnlyShellsAndMonitorsRunningIsDone(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "start the dev server"})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "background_tasks": tasksRunning("shell", "monitor")})
	assert.Equal(t, []string{"work:p1=working", "sleep:p1:1=sleeping"}, stateChanges(r), "a dev server can run for good")
}

func TestStop_WhileWaitingWithAWorkflowRunningGoesOnWorking(t *testing.T) {
	p, r, _ := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "clean up"})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	hook(t, p, map[string]any{"hook_event_name": "Stop", "background_tasks": tasksRunning("workflow")})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func TestReconcile_WaitingIsNeedsInputWhenHooksAreStale(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
	p.listLive = func(context.Context) ([]liveSession, error) {
		return []liveSession{{SessionID: "s1", Status: "waiting", WaitingFor: "permission prompt"}}, nil
	}
	c.advance(hookFresh + time.Second)
	p.reconcile(context.Background())
	assert.Equal(t, core.StateNeedsInput, r.snapshots[0][0].State, "a wait the hooks missed")

	// Claude moves on, and its first new call says so.
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_use_id": "t1", "tool_input": map[string]any{}})
	assert.Equal(t, []string{"work:p1=working", "work:p1:1=working"}, stateChanges(r))
}

func TestReconcile_CorrectsAWaitThatEndedWithoutAHook(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	p.listLive = func(context.Context) ([]liveSession, error) {
		return []liveSession{{SessionID: "s1", Status: "busy"}}, nil // granted; the tool runs long
	}
	c.advance(hookFresh + time.Second)
	p.reconcile(context.Background())
	assert.Equal(t, core.StateWorking, r.snapshots[0][0].State)

	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "t1", "tool_input": rmBuild})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input"}, stateChanges(r), "already working: nothing to report")
}

func TestReconcile_AWaitOnlyItSawIsNotEndedByAnEarlierTool(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	status := "busy" // granted; the tool runs long
	p.listLive = func(context.Context) ([]liveSession, error) {
		return []liveSession{{SessionID: "s1", Status: status, WaitingFor: "dialog open"}}, nil
	}
	c.advance(hookFresh + time.Second)
	p.reconcile(context.Background())
	status = "waiting" // and meanwhile a dialog opens
	p.reconcile(context.Background())
	require.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State)

	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "t1", "tool_input": rmBuild})
	assert.Equal(t, core.StateNeedsInput, p.sessions["s1"].info.State, "the tool finishing does not answer the dialog")
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input"}, stateChanges(r))
}

func TestStateOf(t *testing.T) {
	assert.Equal(t, core.StateWorking, stateOf("busy"))
	assert.Equal(t, core.StateNeedsInput, stateOf("waiting"))
	assert.Equal(t, core.StateSleeping, stateOf("idle"))
	assert.Equal(t, core.StateSleeping, stateOf(""))
}

func TestExpire_RunsWhileNeedsInput(t *testing.T) {
	p, r, c := newTestProvider(t)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p", "prompt": "go"})
	require.NoError(t, p.Deliver(context.Background(), "s1", core.Command{ID: 1, Text: "a"}))
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": rmBuild})
	c.advance(untakenTimeout + time.Second)
	p.expire()
	assert.ErrorIs(t, r.settled[1], errUntaken)
}
