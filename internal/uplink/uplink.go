package uplink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
	"github.com/stonksfi/room-connector/internal/model/room"
	"github.com/stonksfi/room-connector/internal/model/room/entities"
	"github.com/stonksfi/room-connector/internal/store"
)

const (
	flushEvery = 500 * time.Millisecond
	maxBatch   = 200
	// maxBatchBytes keeps a push well under room-be's 4 MB request limit.
	maxBatchBytes = 1 << 20
	maxBackoff    = time.Minute
	// unauthorized is the retry cadence once room-be rejects the token.
	unauthorized = 5 * time.Minute
	// loggedOut is the retry cadence before login, so a fresh `roomd login`
	// is picked up within seconds.
	loggedOut = 5 * time.Second
	// shutdownFlush bounds the last attempt to report outcomes on exit.
	shutdownFlush = 2 * time.Second
	// answerTimeout bounds the screen answer RPC: room-be holds the phone's
	// request for 20s, so a later answer is no use.
	answerTimeout = 5 * time.Second
	// restartedFailure is reported for a command a previous roomd handed to an
	// agent without learning its fate: re-running it could do the work twice.
	restartedFailure = "roomd restarted before the message was confirmed; send it again if it did not run"
)

type sessionKey struct {
	provider string
	id       string
}

// Status is what `roomd status` shows.
type Status struct {
	Queued          int    `json:"queued"`
	StreamConnected bool   `json:"stream_connected"`
	LastError       string `json:"last_error,omitempty"`
}

// Uplink is the core.Sink every provider reports to. It batches what they
// observe to room-be, keeps events on disk until room-be accepts them, and
// hands the commands arriving on the connector stream to the right provider
// and the screen offers to the screen host.
type Uplink struct {
	client    *Client
	queue     *store.Queue
	commands  *store.CommandLog
	providers map[string]core.Provider
	screen    core.ScreenHost // nil where this build cannot show the screen
	wake      chan struct{}

	mu             sync.Mutex
	upserts        map[sessionKey]core.Session
	snapshots      map[string][]core.Session
	snapshotDirty  bool
	pendingRefresh bool // list pending commands at the next flush
	connected      bool
	lastErr        string
	// endView ends the latest screen offer's view: a newer offer ends it,
	// and so does room-be refusing its answer.
	endView context.CancelFunc
}

var _ core.Sink = (*Uplink)(nil)

func New(client *Client, queue *store.Queue, commands *store.CommandLog, providers []core.Provider, screen core.ScreenHost) *Uplink {
	byName := make(map[string]core.Provider, len(providers))
	for _, p := range providers {
		byName[p.Name()] = p
	}
	return &Uplink{
		client:    client,
		queue:     queue,
		commands:  commands,
		providers: byName,
		screen:    screen,
		wake:      make(chan struct{}, 1),
		upserts:   map[sessionKey]core.Session{},
		snapshots: map[string][]core.Session{},
	}
}

func (u *Uplink) poke() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *Uplink) UpsertSession(s core.Session) {
	u.mu.Lock()
	u.upserts[sessionKey{s.Provider, s.ProviderSessionID}] = s
	u.mu.Unlock()
	u.poke()
}

func (u *Uplink) Emit(e core.Event) {
	if err := u.queue.Push(e); err != nil {
		log.Printf("queue event: %v", err)
	}
	u.poke()
}

func (u *Uplink) Snapshot(provider string, live []core.Session) {
	u.mu.Lock()
	u.snapshots[provider] = live
	u.snapshotDirty = true
	u.mu.Unlock()
	u.poke()
}

func (u *Uplink) Settle(commandID int64, err error) {
	s := store.PendingSettle{CommandID: commandID}
	if err != nil {
		s.Failure = core.Truncate(err.Error(), 500)
	}
	if err := u.commands.AddSettle(s); err != nil {
		log.Printf("record outcome of command %d: %v", commandID, err)
	}
	u.poke()
}

// Status reports the uplink's health.
func (u *Uplink) Status() Status {
	queued := u.queue.Len()
	u.mu.Lock()
	defer u.mu.Unlock()
	return Status{Queued: queued, StreamConnected: u.connected, LastError: u.lastErr}
}

func (u *Uplink) setErr(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err == nil {
		u.lastErr = ""
		return
	}
	u.lastErr = err.Error()
}

// Run flushes to room-be and holds the connector stream until ctx ends.
func (u *Uplink) Run(ctx context.Context) {
	go u.downlink(ctx)
	defer u.finalFlush()
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	backoff := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.wake:
		case <-tick.C:
		}
		err := u.flush(ctx)
		u.setErr(err)
		u.stopScreenIfRevoked(err)
		if err == nil {
			backoff = 0
			continue
		}
		backoff = nextBackoff(backoff, err)
		if !sleep(ctx, backoff) {
			return
		}
	}
}

// finalFlush makes one short attempt to report outcomes before exiting, so a
// delivered message is not left pending.
func (u *Uplink) finalFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownFlush) // the run ctx is already done
	defer cancel()
	if u.client.HasToken() {
		if err := u.flushSettles(ctx); err != nil {
			log.Printf("report outcomes on exit: %v", err)
		}
	}
}

func nextBackoff(prev time.Duration, err error) time.Duration {
	switch {
	case errors.Is(err, store.ErrNoToken):
		return loggedOut
	case errors.Is(err, ErrUnauthorized):
		return unauthorized
	case prev == 0:
		return time.Second
	}
	return min(prev*2, maxBackoff)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// flush sends sessions, then events, then command acks. Sessions go first so
// an event's state change is applied after the metadata that precedes it.
func (u *Uplink) flush(ctx context.Context) error {
	if !u.client.HasToken() {
		if u.queue.Len() > 0 || u.hasPending() {
			return store.ErrNoToken
		}
		return nil
	}
	if err := u.flushSessions(ctx); err != nil {
		return err
	}
	if err := u.flushEvents(ctx); err != nil {
		return err
	}
	if err := u.flushSettles(ctx); err != nil {
		return err
	}
	return u.refreshPending(ctx)
}

func (u *Uplink) hasPending() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.upserts) > 0 || u.snapshotDirty
}

func (u *Uplink) flushSessions(ctx context.Context) error {
	u.mu.Lock()
	upserts := u.upserts
	u.upserts = map[sessionKey]core.Session{}
	// Room ends every session a full snapshot leaves out, across providers,
	// so one is sent only once every provider has reported its live sessions.
	full := u.snapshotDirty && len(u.snapshots) == len(u.providers)
	merged := map[sessionKey]core.Session{}
	if full {
		u.snapshotDirty = false
		for _, live := range u.snapshots {
			for _, s := range live {
				merged[sessionKey{s.Provider, s.ProviderSessionID}] = s
			}
		}
	}
	u.mu.Unlock()

	if !full && len(upserts) == 0 {
		return nil
	}
	for k, s := range upserts { // reported after the snapshot was taken
		merged[k] = s
	}
	params := &room.SyncSessionsRequestParams{IsFullSnapshot: &full}
	for _, s := range merged {
		params.Sessions = append(params.Sessions, toUpsert(s))
	}
	err := u.client.SyncSessions(ctx, params)
	if IsPermanent(err) {
		// Resending cannot help; the next snapshot reports live sessions again.
		log.Printf("room refused a session sync, dropping it: %v", err)
		return nil
	}
	if err != nil {
		u.mu.Lock()
		for k, s := range upserts {
			if _, newer := u.upserts[k]; !newer {
				u.upserts[k] = s
			}
		}
		if full {
			u.snapshotDirty = true
		}
		u.mu.Unlock()
		return fmt.Errorf("sync sessions: %w", err)
	}
	return nil
}

// nextBatch takes the oldest events that fit one push.
func (u *Uplink) nextBatch() []store.Queued {
	candidates := u.queue.Peek(maxBatch)
	size := 0
	for i, q := range candidates {
		raw, _ := json.Marshal(toEventInput(q.Event)) // marshalling plain structs cannot fail
		size += len(raw)
		if size > maxBatchBytes && i > 0 {
			return candidates[:i]
		}
	}
	return candidates
}

func (u *Uplink) flushEvents(ctx context.Context) error {
	for {
		batch := u.nextBatch()
		if len(batch) == 0 {
			return nil
		}
		err := u.push(ctx, batch)
		if IsPermanent(err) && len(batch) > 1 {
			// Find the one event room-be refuses by sending the rest singly.
			for _, q := range batch {
				if err := u.pushOne(ctx, q); err != nil {
					return err
				}
			}
			continue
		}
		if IsPermanent(err) {
			log.Printf("room refused event %s, dropping it: %v", batch[0].Event.ClientEventID, err)
			err = nil
		}
		if err != nil {
			return fmt.Errorf("push events: %w", err)
		}
		if err := u.queue.Drop(batch[len(batch)-1].Seq); err != nil {
			return fmt.Errorf("drop sent events: %w", err)
		}
	}
}

func (u *Uplink) pushOne(ctx context.Context, q store.Queued) error {
	err := u.push(ctx, []store.Queued{q})
	if IsPermanent(err) {
		log.Printf("room refused event %s, dropping it: %v", q.Event.ClientEventID, err)
		err = nil
	}
	if err != nil {
		return fmt.Errorf("push events: %w", err)
	}
	return u.queue.Drop(q.Seq)
}

func (u *Uplink) push(ctx context.Context, batch []store.Queued) error {
	params := &room.PushSessionEventsRequestParams{}
	for _, q := range batch {
		params.Events = append(params.Events, toEventInput(q.Event))
	}
	return u.client.PushEvents(ctx, params)
}

func (u *Uplink) flushSettles(ctx context.Context) error {
	for _, s := range u.commands.Settles() {
		params := &room.AckCommandRequestParams{CommandId: &s.CommandID}
		status := entities.CommandStatus_DELIVERED
		if s.Failure != "" {
			status = entities.CommandStatus_FAILED
			params.FailureReason = &s.Failure
		}
		params.Status = &status
		err := u.client.AckCommand(ctx, params)
		if IsPermanent(err) {
			log.Printf("room refused the outcome of command %d, dropping it: %v", s.CommandID, err)
			err = nil
		}
		if err != nil {
			return fmt.Errorf("ack command %d: %w", s.CommandID, err)
		}
		if err := u.commands.Acked(s.CommandID, time.Now()); err != nil {
			return fmt.Errorf("record ack of command %d: %w", s.CommandID, err)
		}
	}
	return nil
}

// refreshPending lists the commands sent while the stream was down, retried
// by the flush loop until it succeeds.
func (u *Uplink) refreshPending(ctx context.Context) error {
	u.mu.Lock()
	want := u.pendingRefresh
	u.mu.Unlock()
	if !want {
		return nil
	}
	pending, err := u.client.ListPendingCommands(ctx)
	if err != nil {
		return fmt.Errorf("list pending commands: %w", err)
	}
	u.mu.Lock()
	u.pendingRefresh = false
	u.mu.Unlock()
	for _, c := range pending.GetCommands() {
		u.dispatch(ctx, c)
	}
	return nil
}

// downlink holds the connector stream open, reconnecting with backoff.
func (u *Uplink) downlink(ctx context.Context) {
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		var err error
		if !u.client.HasToken() {
			err = store.ErrNoToken
		} else {
			err = u.client.Stream(ctx, func(event string, data []byte) {
				u.onStreamEvent(ctx, event, data)
			})
		}
		u.mu.Lock()
		u.connected = false
		u.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 0 // the server ended the stream; reconnect at once
			continue
		}
		if !errors.Is(err, store.ErrNoToken) {
			log.Printf("connector stream: %v", err)
		}
		u.stopScreenIfRevoked(err)
		backoff = nextBackoff(backoff, err)
		if !sleep(ctx, backoff) {
			return
		}
	}
}

func (u *Uplink) onStreamEvent(ctx context.Context, event string, data []byte) {
	switch event {
	case "ready":
		// Anything sent while disconnected is still pending; the flush loop
		// lists it, retrying until it succeeds.
		u.mu.Lock()
		u.connected = true
		u.pendingRefresh = true
		u.mu.Unlock()
		u.poke()
	case "command":
		var c entities.Command
		if err := json.Unmarshal(data, &c); err != nil {
			log.Printf("decode command event: %v", err)
			return
		}
		u.dispatch(ctx, &c)
	case "screen_offer":
		if u.screen == nil {
			return
		}
		var o entities.ScreenOffer
		if err := json.Unmarshal(data, &o); err != nil {
			log.Printf("decode screen offer event: %v", err)
			return
		}
		// The view's context is made here, so offers replace each other in
		// the order they arrive; the answering never blocks the stream reader.
		view, end := u.newView(ctx)
		go u.answerScreen(ctx, view, end, toScreenOffer(&o))
	}
}

// newView gives the next screen view its own context and ends the previous
// one, keeping one view context rather than one per view.
func (u *Uplink) newView(ctx context.Context) (context.Context, context.CancelFunc) {
	view, end := context.WithCancel(ctx)
	u.mu.Lock()
	prev := u.endView
	u.endView = end
	u.mu.Unlock()
	if prev != nil {
		prev()
	}
	return view, end
}

// answerScreen starts a view for offer and hands room-be its answer, or why
// there is none.
// The view runs on view, so ending it (end) never ends a newer one.
func (u *Uplink) answerScreen(ctx, view context.Context, end context.CancelFunc, offer core.ScreenOffer) {
	if !u.client.HasToken() {
		log.Printf("%s: ignored: this machine is not linked", offer)
		end()
		return
	}
	sdp, err := u.screen.Answer(view, offer)
	if err == nil && len(sdp) > core.MaxAnswerSDP {
		end()
		err = fmt.Errorf("the answer is %d bytes, over Room's limit", len(sdp))
	}
	params := &room.AnswerScreenViewRequestParams{OfferId: &offer.ID}
	if err != nil {
		log.Printf("%s: no view: %v", offer, err)
		reason := toUnavailable(err)
		params.Unavailable = &reason
	} else {
		params.AnswerSdp = &sdp
	}
	rpc, cancel := context.WithTimeout(ctx, answerTimeout)
	defer cancel()
	err = u.client.AnswerScreenView(rpc, params)
	switch {
	case err == nil:
	case IsPermanent(err):
		// Refused for good (a 404: nobody waits any more), so nobody will
		// connect to this view. Other failures leave it to the view's own
		// connect timeout, since the answer may have got through.
		log.Printf("%s: room refused the answer: %v", offer, err)
		end()
	default:
		log.Printf("%s: send the answer: %v", offer, err)
		u.stopScreenIfRevoked(err)
	}
}

// stopScreenIfRevoked ends the screen view once room-be rejects this
// device's token: nobody may keep watching a revoked machine.
func (u *Uplink) stopScreenIfRevoked(err error) {
	if u.screen != nil && errors.Is(err, ErrUnauthorized) {
		u.screen.Stop()
	}
}

// dispatch hands a pending command to its provider, once across restarts.
func (u *Uplink) dispatch(ctx context.Context, c *entities.Command) {
	if c.GetStatus() != entities.CommandStatus_PENDING {
		return
	}
	id := c.GetId()
	if u.commands.Inherited(id) {
		u.Settle(id, errors.New(restartedFailure))
		return
	}
	first, err := u.commands.MarkDispatched(id)
	if err != nil {
		log.Printf("record dispatch of command %d: %v", id, err)
	}
	if !first {
		return
	}
	p, ok := u.providers[c.GetProvider()]
	if !ok {
		u.Settle(id, fmt.Errorf("this machine runs no %q provider", c.GetProvider()))
		return
	}
	cmd := core.Command{ID: id, Provider: c.GetProvider(), ProviderSessionID: c.GetProviderSessionId(), Text: c.GetText()}
	if err := p.Deliver(ctx, c.GetProviderSessionId(), cmd); err != nil {
		u.Settle(id, err)
	}
}

func toScreenOffer(o *entities.ScreenOffer) core.ScreenOffer {
	out := core.ScreenOffer{ID: o.GetId(), SDP: o.GetSdp()}
	for _, s := range o.GetIceServers() {
		out.ICEServers = append(out.ICEServers, core.ICEServer{URLs: s.GetUrls(), Username: s.GetUsername(), Credential: s.GetCredential()})
	}
	return out
}

func toUnavailable(err error) entities.ScreenUnavailable {
	switch {
	case errors.Is(err, core.ErrNeedsScreenRecording):
		return entities.ScreenUnavailable_SCREEN_NEEDS_SCREEN_RECORDING
	case errors.Is(err, core.ErrNeedsAccessibility):
		return entities.ScreenUnavailable_SCREEN_NEEDS_ACCESSIBILITY
	default:
		return entities.ScreenUnavailable_SCREEN_FAILED
	}
}

func toUpsert(s core.Session) *entities.SessionUpsert {
	state := toState(s.State)
	title := core.Truncate(s.Title, core.MaxTitle)
	cwd := core.Truncate(s.Cwd, core.MaxCwd)
	out := &entities.SessionUpsert{
		Provider:          &s.Provider,
		ProviderSessionId: &s.ProviderSessionID,
		Title:             &title,
		Cwd:               &cwd,
		State:             &state,
	}
	if !s.StartedAt.IsZero() {
		v := s.StartedAt.Unix()
		out.StartedAt = &v
	}
	if !s.LastActivityAt.IsZero() {
		v := s.LastActivityAt.Unix()
		out.LastActivityAt = &v
	}
	return out
}

func toEventInput(e core.Event) *entities.SessionEventInput {
	kind := toKind(e.Kind)
	out := &entities.SessionEventInput{
		Provider:          &e.Provider,
		ProviderSessionId: &e.ProviderSessionID,
		ClientEventId:     &e.ClientEventID,
		Kind:              &kind,
	}
	if e.Text != "" {
		text := core.Truncate(e.Text, core.MaxEventText)
		out.Text = &text
	}
	if e.ToolName != "" {
		tool := core.Truncate(e.ToolName, core.MaxToolName)
		out.ToolName = &tool
	}
	if e.Kind == core.KindStateChange {
		state := toState(e.State)
		out.State = &state
	}
	if e.CommandID != 0 {
		out.CommandId = &e.CommandID
	}
	if !e.OccurredAt.IsZero() {
		v := e.OccurredAt.Unix()
		out.OccurredAt = &v
	}
	return out
}

func toState(s core.State) entities.SessionState {
	switch s {
	case core.StateWorking:
		return entities.SessionState_WORKING
	case core.StateEnded:
		return entities.SessionState_ENDED
	default:
		return entities.SessionState_SLEEPING
	}
}

func toKind(k core.EventKind) entities.SessionEventKind {
	switch k {
	case core.KindAssistantMessage:
		return entities.SessionEventKind_ASSISTANT_MESSAGE
	case core.KindToolActivity:
		return entities.SessionEventKind_TOOL_ACTIVITY
	case core.KindStateChange:
		return entities.SessionEventKind_STATE_CHANGE
	default:
		return entities.SessionEventKind_USER_MESSAGE
	}
}
