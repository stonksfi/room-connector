// Package claudecode is Room's provider for Claude Code. State and
// conversation come from the Room plugin's hooks; messages from the app reach
// a session through the plugin's in-session script, which polls this
// provider's inbox and submits them as the person's own prompt, and answers
// to the session's questions through the same script's wait (answers.go).
package claudecode

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// Name is the provider id Room stores.
const Name = "claude_code"

const (
	// reconcileEvery is how often live sessions are checked with `claude agents`.
	reconcileEvery = 15 * time.Second
	// hookFresh is how long hook-derived state outranks `claude agents`, which
	// can lag a turn that just started or ended.
	hookFresh = 20 * time.Second
	// pollFresh is how recently the in-session script must have polled for the
	// session to count as reachable by it.
	pollFresh = 10 * time.Second
	// untakenTimeout fails a message no in-session script picked up while the
	// session slept or waited on the person (a busy session's Stop hook can
	// still hand it over).
	untakenTimeout = 2 * time.Minute
	// unconfirmedTimeout fails a message taken but never seen as a prompt,
	// counted only while the session sleeps or waits on the person (a long
	// turn delays it legitimately).
	unconfirmedTimeout = 2 * time.Minute
	// remembered bounds the ids a session keeps of each kind: its finished
	// replies, so late chunks of one are ignored rather than reported again,
	// and the tool calls its turn started.
	remembered = 32
)

var (
	errUntaken     = errors.New("the session did not pick the message up: is the Room plugin enabled in it?")
	errUnconfirmed = errors.New("the session took the message but never ran it")
	errEnded       = errors.New("the session ended before the message was delivered")
)

type queued struct {
	cmd core.Command
	at  time.Time
	// viaStop marks a message handed to Claude in a Stop hook's answer; it is
	// delivered once Claude is seen continuing.
	viaStop bool
}

// session is everything the provider tracks about one Claude Code session.
type session struct {
	info       core.Session
	lastHookAt time.Time
	lastPollAt time.Time
	// promptID is the turn in progress, stops counts its Stop hooks, and
	// assistantSent records whether the reply since the last Stop was reported.
	promptID      string
	stops         int
	assistantSent bool
	// stopped is set when a turn ended at Stop and cleared by the next
	// prompt: a reply whose chunks complete in between was already reported
	// from the Stop's own text.
	stopped bool
	// changes counts the turn's state changes after its start, so each has
	// an id of its own. tools are the tool calls the turn started, newest
	// last (a question's with its fingerprint). waitTool is, while the
	// session needs input, the fingerprint of the tool the person is asked
	// about ("" when there is none).
	changes  int
	tools    []toolCall
	waitTool string
	drafts   map[string]*draft
	closed   []string // message ids already reported, newest last
	inbox    []queued // waiting for the in-session script
	handed   []queued // taken, waiting to be seen running
	// Answers from Room (answers.go).
	takesAnswers bool                  // its script said so on its last poll
	answers      []pendingAnswer       // queued or handed over
	waits        map[string]*waitState // the script's waits, by question print
	closedAsks   []string              // prints whose question closed this turn
	wake         chan struct{}         // closed to wake the waits
}

// toolCall is one tool use a turn started; fingerprint is kept for a
// question only.
type toolCall struct {
	id, fingerprint string
}

func (s *session) isClosed(messageID string) bool {
	return slices.Contains(s.closed, messageID)
}

func (s *session) close(messageID string) {
	s.closed = remember(s.closed, messageID)
}

// remember appends v, keeping only the newest `remembered` entries.
func remember[T any](list []T, v T) []T {
	list = append(list, v)
	if len(list) > remembered {
		list = list[len(list)-remembered:]
	}
	return list
}

type Provider struct {
	claudePath string
	now        func() time.Time
	listLive   func(ctx context.Context) ([]liveSession, error)
	answerHold time.Duration

	mu       sync.Mutex
	sink     core.Sink
	sessions map[string]*session
	lastErr  string // last reconcile failure, logged once per change
}

var (
	_ core.Provider     = (*Provider)(nil)
	_ core.HookReceiver = (*Provider)(nil)
	_ core.InboxServer  = (*Provider)(nil)
	_ core.AnswerServer = (*Provider)(nil)
)

// New returns the provider. claudePath is the claude binary used to list live
// sessions; empty means look it up on PATH.
func New(claudePath string) *Provider {
	p := &Provider{
		claudePath: claudePath,
		now:        time.Now,
		sessions:   map[string]*session{},
		answerHold: answerHold,
	}
	p.listLive = p.listWithCLI
	return p
}

func (p *Provider) Name() string {
	return Name
}

// Start reconciles live sessions and expires undelivered messages until ctx ends.
func (p *Provider) Start(ctx context.Context, sink core.Sink) error {
	p.mu.Lock()
	p.sink = sink
	p.mu.Unlock()

	p.reconcile(ctx)
	reconcile := time.NewTicker(reconcileEvery)
	defer reconcile.Stop()
	expire := time.NewTicker(5 * time.Second)
	defer expire.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reconcile.C:
			p.reconcile(ctx)
		case <-expire.C:
			p.expire()
		}
	}
}

// Deliver queues a message for the session's in-session script, or an answer
// for the wait on its question.
func (p *Provider) Deliver(ctx context.Context, providerSessionID string, cmd core.Command) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.get(providerSessionID)
	if cmd.Kind == core.CommandAnswer {
		return p.deliverAnswer(s, cmd, p.now())
	}
	s.inbox = append(s.inbox, queued{cmd: cmd, at: p.now()})
	return nil
}

// TakeInbox hands the session's queued messages to its in-session script and
// records that the script is alive.
func (p *Provider) TakeInbox(providerSessionID string) []core.InboxMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.get(providerSessionID)
	now := p.now()
	s.lastPollAt = now
	out := make([]core.InboxMessage, 0, len(s.inbox))
	for _, q := range s.inbox {
		out = append(out, core.InboxMessage{CommandID: q.cmd.ID, Text: q.cmd.Text})
		s.handed = append(s.handed, queued{cmd: q.cmd, at: now})
	}
	s.inbox = nil
	return out
}

// ReturnInbox fails a message the in-session script took but could not submit.
func (p *Provider) ReturnInbox(providerSessionID string, commandID int64, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[providerSessionID]
	if !ok || p.sink == nil {
		return
	}
	for i, q := range s.handed {
		if q.cmd.ID == commandID {
			s.handed = append(s.handed[:i], s.handed[i+1:]...)
			p.sink.Settle(commandID, errors.New("the session refused the message: "+reason))
			return
		}
	}
}

// get returns the tracked session, starting to track it if new; the caller
// holds p.mu.
func (p *Provider) get(id string) *session {
	s, ok := p.sessions[id]
	if !ok {
		now := p.now()
		s = &session{
			info: core.Session{
				Provider:          Name,
				ProviderSessionID: id,
				State:             core.StateSleeping,
				StartedAt:         now,
				LastActivityAt:    now,
			},
			drafts: map[string]*draft{},
			waits:  map[string]*waitState{},
		}
		p.sessions[id] = s
	}
	return s
}

// forget stops tracking a session and fails its undelivered messages; the
// caller holds p.mu.
func (p *Provider) forget(id string) {
	s, ok := p.sessions[id]
	if !ok {
		return
	}
	for _, q := range append(s.inbox, s.handed...) {
		p.sink.Settle(q.cmd.ID, errEnded)
	}
	for _, a := range s.answers {
		p.sink.Settle(a.cmd.ID, errEnded)
	}
	s.answers = nil
	s.wakeWaits() // its waits see the session gone and end
	delete(p.sessions, id)
}

// expire fails messages that are not going to land.
func (p *Provider) expire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sink == nil {
		return
	}
	now := p.now()
	for _, s := range p.sessions {
		p.expireAnswers(s, now)
		if s.info.State == core.StateWorking {
			continue // a turn is running: its end (or Stop hook) still delivers
		}
		s.inbox = p.expireList(s.inbox, func(q queued) bool {
			return now.Sub(q.at) > untakenTimeout && now.Sub(s.lastPollAt) > pollFresh
		}, errUntaken)
		s.handed = p.expireList(s.handed, func(q queued) bool {
			return now.Sub(q.at) > unconfirmedTimeout
		}, errUnconfirmed)
	}
}

func (p *Provider) expireList(list []queued, expired func(queued) bool, reason error) []queued {
	kept := list[:0]
	for _, q := range list {
		if expired(q) {
			p.sink.Settle(q.cmd.ID, reason)
			continue
		}
		kept = append(kept, q)
	}
	return kept
}

// logReconcileErr logs a reconcile failure once per distinct error; the
// caller holds p.mu.
func (p *Provider) logReconcileErr(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != p.lastErr && msg != "" {
		log.Printf("claude_code: listing live sessions failed, skipping reconcile: %v", err)
	}
	p.lastErr = msg
}

// titleFor names a session after its folder when it has no name of its own.
func titleFor(name, cwd string) string {
	if name != "" {
		return name
	}
	if cwd != "" {
		return filepath.Base(cwd)
	}
	return ""
}
