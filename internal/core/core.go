// Package core is the provider-neutral contract between roomd and the agents
// it watches. Nothing here names an agent: Claude Code is one Provider, and
// another agent (Codex, ...) is another implementation of the same interface.
package core

import (
	"context"
	"time"
	"unicode/utf8"
)

// State is a session's lifecycle as Room shows it.
type State string

const (
	StateSleeping State = "sleeping"
	StateWorking  State = "working"
	StateEnded    State = "ended"
)

// EventKind is the kind of one conversation row.
type EventKind string

const (
	KindUserMessage      EventKind = "user_message"
	KindAssistantMessage EventKind = "assistant_message"
	KindToolActivity     EventKind = "tool_activity"
	KindStateChange      EventKind = "state_change"
)

// Session is what a provider knows about one of its sessions.
type Session struct {
	Provider          string
	ProviderSessionID string
	Title             string
	Cwd               string
	State             State
	StartedAt         time.Time
	LastActivityAt    time.Time
}

// Event is one conversation row or state change of a session.
type Event struct {
	Provider          string
	ProviderSessionID string
	// ClientEventID is stable for the row, so resending it is harmless.
	ClientEventID string
	Kind          EventKind
	Text          string
	ToolName      string
	// State is set for KindStateChange only.
	State State
	// CommandID links a user message to the Room command it delivered; 0 if none.
	CommandID  int64
	OccurredAt time.Time
}

// Command is a message the person sent from the app to a session.
type Command struct {
	ID                int64
	Provider          string
	ProviderSessionID string
	Text              string
}

// Sink receives what providers observe. The uplink implements it.
type Sink interface {
	// UpsertSession reports a session's current metadata and state.
	UpsertSession(s Session)
	// Emit reports one event.
	Emit(e Event)
	// Snapshot reports every live session of the provider, so sessions that
	// vanished without saying goodbye are ended.
	Snapshot(provider string, live []Session)
	// Settle reports a command's fate: delivered when err is nil.
	Settle(commandID int64, err error)
}

// Provider adapts one agent to Room.
type Provider interface {
	// Name is the provider id sent to Room, e.g. "claude_code".
	Name() string
	// Start watches the agent until ctx ends, reporting to sink.
	Start(ctx context.Context, sink Sink) error
	// Deliver queues cmd for the session. It returns once the command is
	// accepted; the provider settles it through the sink when it lands.
	Deliver(ctx context.Context, providerSessionID string, cmd Command) error
}

// HookReceiver is a provider whose in-session hooks report to roomd over its
// local socket. The response is handed back to the hook verbatim.
type HookReceiver interface {
	HandleHook(ctx context.Context, body []byte) ([]byte, error)
}

// InboxMessage is a queued message handed to an in-session script.
type InboxMessage struct {
	CommandID int64  `json:"command_id"`
	Text      string `json:"text"`
}

// InboxServer is a provider whose in-session script polls roomd for the
// messages queued for its session.
type InboxServer interface {
	TakeInbox(providerSessionID string) []InboxMessage
	// ReturnInbox reports that the script could not submit a message it took.
	ReturnInbox(providerSessionID string, commandID int64, reason string)
}

// Limits Room's API enforces. The uplink holds every provider to them, so one
// oversized row can never stall the queue.
const (
	MaxEventText = 100_000
	MaxTitle     = 200
	MaxCwd       = 4096
	MaxToolName  = 128
)

// Truncate cuts s to at most max bytes on a character boundary, marking the cut.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const mark = "…"
	cut := max - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}
