package uplink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stonksfi/room-connector/internal/core"
	"github.com/stonksfi/room-connector/internal/model/room/entities"
	"github.com/stonksfi/room-connector/internal/store"
)

// fakeRoom records RPC bodies and answers success, or the status set for a
// path, or 400 for an event whose text is "bad".
type fakeRoom struct {
	mu      sync.Mutex
	bodies  map[string][]map[string]any
	status  int
	byPath  map[string]int
	pending string // data of ListPendingCommands
}

func (f *fakeRoom) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.bodies[r.URL.Path] = append(f.bodies[r.URL.Path], body)
	status := f.status
	if st, ok := f.byPath[r.URL.Path]; ok {
		status = st
	}
	pending := f.pending
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "rmd_test" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if strings.Contains(string(raw), `"text":"bad"`) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"message":"Bad Request"}`))
		return
	}
	w.WriteHeader(status)
	data := "{}"
	if r.URL.Path == "/api/v1/connector/commands" && pending != "" {
		data = pending
	}
	_, _ = w.Write([]byte(`{"code":0,"message":"success","data":` + data + `}`))
}

func (f *fakeRoom) calls(path string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

type fakeProvider struct {
	name      string
	delivered []core.Command
	err       error
}

func (p *fakeProvider) Name() string                           { return p.name }
func (p *fakeProvider) Start(context.Context, core.Sink) error { return nil }
func (p *fakeProvider) Deliver(_ context.Context, _ string, c core.Command) error {
	p.delivered = append(p.delivered, c)
	return p.err
}

func newTestUplink(t *testing.T, token string, providers ...core.Provider) (*Uplink, *fakeRoom) {
	t.Helper()
	room := &fakeRoom{bodies: map[string][]map[string]any{}, status: http.StatusOK, byPath: map[string]int{}}
	srv := httptest.NewServer(room)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	q, err := store.OpenQueue(filepath.Join(dir, "q.jsonl"))
	require.NoError(t, err)
	t.Cleanup(func() { q.Close() })
	cmds, err := store.OpenCommandLog(filepath.Join(dir, "commands.json"))
	require.NoError(t, err)
	return New(NewClient(srv.URL, func() string { return token }), q, cmds, providers, nil), room
}

func TestFlush_SendsSessionsThenEventsThenAcks(t *testing.T) {
	cc := &fakeProvider{name: "claude_code"}
	u, room := newTestUplink(t, "rmd_test", cc)
	now := time.Unix(1_800_000_000, 0)
	u.UpsertSession(core.Session{Provider: "claude_code", ProviderSessionID: "s1", Title: "t", State: core.StateWorking, StartedAt: now})
	u.Emit(core.Event{Provider: "claude_code", ProviderSessionID: "s1", ClientEventID: "e1", Kind: core.KindStateChange, State: core.StateWorking, CommandID: 4, OccurredAt: now})
	u.Emit(core.Event{Provider: "claude_code", ProviderSessionID: "s1", ClientEventID: "e2", Kind: core.KindToolActivity, ToolName: "Bash", Text: "ls"})
	u.Settle(4, nil)
	u.Settle(5, assert.AnError)

	require.NoError(t, u.flush(context.Background()))

	sync := room.calls("/api/v1/connector/sessions/sync")
	require.Len(t, sync, 1)
	params := sync[0]["params"].(map[string]any)
	assert.Equal(t, false, params["is_full_snapshot"])
	sess := params["sessions"].([]any)[0].(map[string]any)
	assert.Equal(t, float64(1), sess["state"]) // WORKING

	events := room.calls("/api/v1/connector/events")
	require.Len(t, events, 1)
	evs := events[0]["params"].(map[string]any)["events"].([]any)
	require.Len(t, evs, 2)
	first := evs[0].(map[string]any)
	assert.Equal(t, float64(3), first["kind"]) // STATE_CHANGE
	assert.Equal(t, float64(1), first["state"])
	assert.Equal(t, float64(4), first["command_id"])
	assert.Equal(t, 0, u.queue.Len(), "accepted events leave the queue")

	acks := room.calls("/api/v1/connector/commands/ack")
	require.Len(t, acks, 2)
	assert.Equal(t, float64(1), acks[0]["params"].(map[string]any)["status"]) // DELIVERED
	failed := acks[1]["params"].(map[string]any)
	assert.Equal(t, float64(2), failed["status"]) // FAILED
	assert.Equal(t, assert.AnError.Error(), failed["failure_reason"])
}

func TestFlush_FullSnapshotWaitsForEveryProvider(t *testing.T) {
	a, b := &fakeProvider{name: "a"}, &fakeProvider{name: "b"}
	u, room := newTestUplink(t, "rmd_test", a, b)
	u.Snapshot("a", []core.Session{{Provider: "a", ProviderSessionID: "1"}})
	require.NoError(t, u.flush(context.Background()))
	assert.Empty(t, room.calls("/api/v1/connector/sessions/sync"), "b has not reported: a full snapshot would end b's sessions")

	u.Snapshot("b", []core.Session{{Provider: "b", ProviderSessionID: "2"}})
	require.NoError(t, u.flush(context.Background()))
	sync := room.calls("/api/v1/connector/sessions/sync")
	require.Len(t, sync, 1)
	params := sync[0]["params"].(map[string]any)
	assert.Equal(t, true, params["is_full_snapshot"])
	assert.Len(t, params["sessions"], 2)
}

func TestFlush_KeepsEverythingWhenRoomFails(t *testing.T) {
	u, room := newTestUplink(t, "rmd_test", &fakeProvider{name: "claude_code"})
	room.status = http.StatusInternalServerError
	u.UpsertSession(core.Session{Provider: "claude_code", ProviderSessionID: "s1"})
	u.Emit(core.Event{Provider: "claude_code", ProviderSessionID: "s1", ClientEventID: "e1"})

	assert.Error(t, u.flush(context.Background()))
	assert.Equal(t, 1, u.queue.Len())
	assert.Len(t, u.upserts, 1, "the upsert is retried")

	room.status = http.StatusOK
	require.NoError(t, u.flush(context.Background()))
	assert.Equal(t, 0, u.queue.Len())
}

func TestFlush_WithoutTokenQueues(t *testing.T) {
	u, room := newTestUplink(t, "", &fakeProvider{name: "claude_code"})
	require.NoError(t, u.flush(context.Background()), "nothing to send is not an error")
	u.Emit(core.Event{ClientEventID: "e1"})
	assert.ErrorIs(t, u.flush(context.Background()), store.ErrNoToken)
	assert.Empty(t, room.calls("/api/v1/connector/events"))
}

func TestFlush_UnauthorizedIsRecognised(t *testing.T) {
	u, _ := newTestUplink(t, "rmd_revoked", &fakeProvider{name: "claude_code"})
	u.Emit(core.Event{ClientEventID: "e1"})
	err := u.flush(context.Background())
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.Equal(t, unauthorized, nextBackoff(time.Second, err))
}

func TestDispatch_RoutesPendingCommandsOnce(t *testing.T) {
	cc := &fakeProvider{name: "claude_code"}
	u, _ := newTestUplink(t, "rmd_test", cc)
	cmd := `{"id":9,"session_id":1,"status":0,"text":"hi","provider":"claude_code","provider_session_id":"s1"}`
	u.onStreamEvent(context.Background(), "command", []byte(cmd))
	u.onStreamEvent(context.Background(), "command", []byte(cmd))
	u.onStreamEvent(context.Background(), "command", []byte(`{"id":10,"status":1,"provider":"claude_code"}`))
	require.Len(t, cc.delivered, 1)
	assert.Equal(t, core.Command{ID: 9, Provider: "claude_code", ProviderSessionID: "s1", Text: "hi"}, cc.delivered[0])

	u.onStreamEvent(context.Background(), "command", []byte(`{"id":11,"status":0,"provider":"codex"}`))
	settles := u.commands.Settles()
	require.Len(t, settles, 1)
	assert.Contains(t, settles[0].Failure, "codex")
}

func TestFlush_DropsWhatRoomPermanentlyRefuses(t *testing.T) {
	u, room := newTestUplink(t, "rmd_test", &fakeProvider{name: "claude_code"})
	u.Emit(core.Event{ClientEventID: "e1", Text: "fine"})
	u.Emit(core.Event{ClientEventID: "e2", Text: "bad"})
	u.Emit(core.Event{ClientEventID: "e3", Text: "fine too"})
	u.Settle(7, nil)
	room.byPath["/api/v1/connector/commands/ack"] = http.StatusForbidden

	require.NoError(t, u.flush(context.Background()), "a refused item never stalls the uplink")
	assert.Equal(t, 0, u.queue.Len())
	assert.Empty(t, u.commands.Settles())
	pushed := []string{}
	for _, call := range room.calls("/api/v1/connector/events") {
		for _, e := range call["params"].(map[string]any)["events"].([]any) {
			pushed = append(pushed, e.(map[string]any)["client_event_id"].(string))
		}
	}
	assert.Equal(t, []string{"e1", "e2", "e3", "e1", "e2", "e3"}, pushed, "the batch is retried singly to find the refused event")
}

func TestToEventInput_HoldsEveryProviderToRoomsLimits(t *testing.T) {
	big := strings.Repeat("x", core.MaxEventText+10)
	in := toEventInput(core.Event{Text: big, ToolName: strings.Repeat("t", 500), Label: strings.Repeat("l", 500)})
	assert.LessOrEqual(t, len(in.GetText()), core.MaxEventText)
	assert.LessOrEqual(t, len(in.GetToolName()), core.MaxToolName)
	assert.LessOrEqual(t, len(in.GetLabel()), core.MaxLabel)
	up := toUpsert(core.Session{Title: strings.Repeat("t", 1000), Cwd: strings.Repeat("c", 5000)})
	assert.LessOrEqual(t, len(up.GetTitle()), core.MaxTitle)
	assert.LessOrEqual(t, len(up.GetCwd()), core.MaxCwd)
}

func TestToEventInput_Note(t *testing.T) {
	in := toEventInput(core.Event{Kind: core.KindNote, Label: "Subagent report", Text: "## Review"})
	assert.Equal(t, entities.SessionEventKind_NOTE, in.GetKind())
	assert.Equal(t, "Subagent report", in.GetLabel())
	assert.Nil(t, toEventInput(core.Event{Kind: core.KindUserMessage}).Label, "no label unless set")
}

func TestNextBatch_CapsBytes(t *testing.T) {
	u, _ := newTestUplink(t, "rmd_test", &fakeProvider{name: "claude_code"})
	text := strings.Repeat("x", core.MaxEventText-10)
	for i := 0; i < 15; i++ {
		u.Emit(core.Event{ClientEventID: "e", Text: text})
	}
	batch := u.nextBatch()
	assert.Less(t, len(batch), 15)
	assert.NotEmpty(t, batch)
}

func TestDispatch_NeverReRunsACommandAfterRestart(t *testing.T) {
	cc := &fakeProvider{name: "claude_code"}
	dir := t.TempDir()
	cmds, err := store.OpenCommandLog(filepath.Join(dir, "commands.json"))
	require.NoError(t, err)
	_, err = cmds.MarkDispatched(9) // handed out by the previous roomd, fate unknown
	require.NoError(t, err)

	restarted, err := store.OpenCommandLog(filepath.Join(dir, "commands.json"))
	require.NoError(t, err)
	q, err := store.OpenQueue(filepath.Join(dir, "q.jsonl"))
	require.NoError(t, err)
	defer q.Close()
	u := New(NewClient("http://unused", func() string { return "" }), q, restarted, []core.Provider{cc}, nil)
	u.dispatch(context.Background(), &entities.Command{Id: ptr(int64(9)), Status: ptr(entities.CommandStatus_PENDING), Provider: ptr("claude_code")})

	assert.Empty(t, cc.delivered)
	settles := restarted.Settles()
	require.Len(t, settles, 1)
	assert.Equal(t, restartedFailure, settles[0].Failure)
}

func TestReady_ListsPendingAtTheNextFlushUntilItWorks(t *testing.T) {
	cc := &fakeProvider{name: "claude_code"}
	u, room := newTestUplink(t, "rmd_test", cc)
	room.pending = `{"commands":[{"id":4,"status":0,"provider":"claude_code","provider_session_id":"s1","text":"hi"}]}`
	room.byPath["/api/v1/connector/commands"] = http.StatusInternalServerError
	u.onStreamEvent(context.Background(), "ready", nil)

	assert.Error(t, u.flush(context.Background()))
	assert.Empty(t, cc.delivered)

	delete(room.byPath, "/api/v1/connector/commands")
	require.NoError(t, u.flush(context.Background()))
	require.Len(t, cc.delivered, 1)
	assert.Equal(t, int64(4), cc.delivered[0].ID)
	require.NoError(t, u.flush(context.Background()))
	assert.Len(t, cc.delivered, 1, "listed once")
}

func ptr[T any](v T) *T { return &v }

func TestReadEvents(t *testing.T) {
	body := "event: ready\ndata: {}\n\n: comment\nevent: command\ndata: {\"id\":1}\n\nevent: x\ndata: a\ndata: b\n\n"
	var got []string
	lines := 0
	require.NoError(t, readEvents(strings.NewReader(body), func() { lines++ }, func(event string, data []byte) {
		got = append(got, event+"="+string(data))
	}))
	assert.Equal(t, []string{"ready={}", `command={"id":1}`, "x=a\nb"}, got)
	assert.Equal(t, 11, lines, "every line feeds the idle watchdog")
}

func TestNextBackoff(t *testing.T) {
	assert.Equal(t, time.Second, nextBackoff(0, assert.AnError))
	assert.Equal(t, 4*time.Second, nextBackoff(2*time.Second, assert.AnError))
	assert.Equal(t, maxBackoff, nextBackoff(maxBackoff, assert.AnError))
	assert.Equal(t, loggedOut, nextBackoff(0, store.ErrNoToken), "a login is picked up within seconds")
	assert.Equal(t, unauthorized, nextBackoff(0, ErrUnauthorized))
}

// fakeHost is a core.ScreenHost that answers every offer the same way and
// keeps each view's context, to see which views were ended.
type fakeHost struct {
	mu     sync.Mutex
	offers []core.ScreenOffer
	views  []context.Context
	stops  int
	sdp    string
	err    error
}

func (h *fakeHost) Answer(ctx context.Context, o core.ScreenOffer) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.offers = append(h.offers, o)
	h.views = append(h.views, ctx)
	return h.sdp, h.err
}

func (h *fakeHost) Stop() { h.mu.Lock(); h.stops++; h.mu.Unlock() }

func (h *fakeHost) stopped() int { h.mu.Lock(); defer h.mu.Unlock(); return h.stops }

func (h *fakeHost) ended(i int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.views[i].Err() != nil
}

const screenOffer = `{"id":"so_1","sdp":"v=0 offer","ice_servers":[{"urls":["turn:turn.example:3478"],"username":"u","credential":"c"}]}`

// answerScreen answers offer as the stream reader would, waiting for it.
func answerScreen(u *Uplink, offer core.ScreenOffer) {
	view, end := u.newView(context.Background())
	u.answerScreen(context.Background(), view, end, offer)
}

func screenAnswers(room *fakeRoom) func() []map[string]any {
	return func() []map[string]any { return room.calls("/api/v1/connector/screen/answer") }
}

func TestScreenOffer_AnswersWithTheHostsSDP(t *testing.T) {
	host := &fakeHost{sdp: "v=0 answer"}
	u, room := newTestUplink(t, "rmd_test")
	u.screen = host
	u.onStreamEvent(context.Background(), "screen_offer", []byte(screenOffer))

	answers := screenAnswers(room)
	require.Eventually(t, func() bool { return len(answers()) == 1 }, time.Second, 5*time.Millisecond)
	params := answers()[0]["params"].(map[string]any)
	assert.Equal(t, map[string]any{"offer_id": "so_1", "answer_sdp": "v=0 answer"}, params)
	require.Len(t, host.offers, 1)
	assert.Equal(t, core.ScreenOffer{ID: "so_1", SDP: "v=0 offer", ICEServers: []core.ICEServer{
		{URLs: []string{"turn:turn.example:3478"}, Username: "u", Credential: "c"},
	}}, host.offers[0])
	assert.False(t, host.ended(0), "the view runs on")
}

func TestScreenOffer_ReportsAMissingPermission(t *testing.T) {
	host := &fakeHost{err: fmt.Errorf("wrapped: %w", core.ErrNeedsAccessibility)}
	u, room := newTestUplink(t, "rmd_test")
	u.screen = host
	answerScreen(u, core.ScreenOffer{ID: "so_1"})

	answers := screenAnswers(room)()
	require.Len(t, answers, 1)
	params := answers[0]["params"].(map[string]any)
	assert.Equal(t, float64(entities.ScreenUnavailable_SCREEN_NEEDS_ACCESSIBILITY), params["unavailable"])
	assert.NotContains(t, params, "answer_sdp")
}

func TestScreenOffer_EndsTheViewNobodyWaitsFor(t *testing.T) {
	host := &fakeHost{sdp: "v=0 answer"}
	u, room := newTestUplink(t, "rmd_test")
	u.screen = host
	room.byPath["/api/v1/connector/screen/answer"] = http.StatusNotFound
	answerScreen(u, core.ScreenOffer{ID: "so_1"})
	assert.True(t, host.ended(0))
	assert.Zero(t, host.stopped(), "only that view ends, never a newer one")
}

func TestScreenOffer_KeepsTheViewWhenTheAnswerMayHaveArrived(t *testing.T) {
	host := &fakeHost{sdp: "v=0 answer"}
	u, room := newTestUplink(t, "rmd_test")
	u.screen = host
	room.byPath["/api/v1/connector/screen/answer"] = http.StatusBadGateway
	answerScreen(u, core.ScreenOffer{ID: "so_1"})
	assert.Len(t, screenAnswers(room)(), 1)
	assert.False(t, host.ended(0), "the view's own connect timeout ends it if nobody comes")
}

func TestScreenOffer_ANewerOfferEndsTheOlderView(t *testing.T) {
	host := &fakeHost{sdp: "v=0 answer"}
	u, _ := newTestUplink(t, "rmd_test")
	u.screen = host
	answerScreen(u, core.ScreenOffer{ID: "so_1"})
	answerScreen(u, core.ScreenOffer{ID: "so_2"})
	assert.True(t, host.ended(0))
	assert.False(t, host.ended(1))
}

func TestScreenOffer_AnOversizedAnswerFails(t *testing.T) {
	host := &fakeHost{sdp: strings.Repeat("a", core.MaxAnswerSDP+1)}
	u, room := newTestUplink(t, "rmd_test")
	u.screen = host
	answerScreen(u, core.ScreenOffer{ID: "so_1"})
	params := screenAnswers(room)()[0]["params"].(map[string]any)
	assert.Equal(t, float64(entities.ScreenUnavailable_SCREEN_FAILED), params["unavailable"])
	assert.True(t, host.ended(0))
}

func TestScreenOffer_IgnoredWhenNotLinkedOrWithoutAHost(t *testing.T) {
	host := &fakeHost{sdp: "v=0 answer"}
	u, room := newTestUplink(t, "")
	u.screen = host
	answerScreen(u, core.ScreenOffer{ID: "so_1"})
	assert.Empty(t, host.offers, "after logout, no view starts")

	u, room = newTestUplink(t, "rmd_test")
	u.onStreamEvent(context.Background(), "screen_offer", []byte(screenOffer)) // returns at once: nothing to answer with
	assert.Empty(t, screenAnswers(room)())
}

func TestDownlink_RevokedDeviceStopsTheView(t *testing.T) {
	host := &fakeHost{}
	u, _ := newTestUplink(t, "rmd_revoked")
	u.screen = host
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.downlink(ctx)
	assert.Eventually(t, func() bool { return host.stopped() > 0 }, 2*time.Second, 5*time.Millisecond)
}
