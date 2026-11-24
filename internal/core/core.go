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
	// StateSleeping is idle until the next message: the turn finished.
	StateSleeping State = "sleeping"
	StateWorking  State = "working"
	StateEnded    State = "ended"
	// StateNeedsInput is blocked on the person: a permission prompt, a
	// question, a dialog.
	StateNeedsInput State = "needs_input"
)

// EventKind is the kind of one conversation row.
type EventKind string

const (
	KindUserMessage      EventKind = "user_message"
	KindAssistantMessage EventKind = "assistant_message"
	KindToolActivity     EventKind = "tool_activity"
	KindStateChange      EventKind = "state_change"
	// KindNote is something the agent's harness handed it in the person's
	// turn (a subagent's report, a background task finishing): read by the
	// agent, not said by the person.
	KindNote EventKind = "note"
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
	// Label names a KindNote as the app shows it ("Subagent report").
	Label string
	// State is set for KindStateChange only.
	State State
	// CommandID links a user message to the Room command it delivered; 0 if none.
	CommandID int64
	// Ask is set on a KindAssistantMessage that asks the person something
	// Room can answer (an AnswerServer's session): its structured form.
	Ask        *Ask `json:",omitempty"`
	OccurredAt time.Time
}

// AskKind is what an Ask asks for.
type AskKind string

const (
	// AskKindQuestion is questions with options to pick from.
	AskKindQuestion AskKind = "question"
	// AskKindPlan is a plan the agent proposes: Room can send feedback to keep
	// planning; approving it is the person's at the machine.
	AskKindPlan AskKind = "plan"
)

// Ask is a question the agent put to the person, as Room can answer it.
type Ask struct {
	Kind AskKind
	// Questions is set for AskKindQuestion only.
	Questions []AskQuestion `json:",omitempty"`
}

type AskQuestion struct {
	Question    string
	Header      string `json:",omitempty"`
	MultiSelect bool   `json:",omitempty"`
	Options     []AskOption
}

type AskOption struct {
	Label       string
	Description string `json:",omitempty"`
}

// CommandKind is what a Command asks of a session.
type CommandKind string

const (
	// CommandMessage is a message to run as the person's next prompt.
	CommandMessage CommandKind = "message"
	// CommandAnswer answers a question the session asked (an Ask).
	CommandAnswer CommandKind = "answer"
)

// Command is something the person sent from the app to a session.
type Command struct {
	ID                int64
	Kind              CommandKind
	Provider          string
	ProviderSessionID string
	// Text is the message, or for an answer a one-line summary of it.
	Text string
	// Answer is set for CommandAnswer only.
	Answer *Answer
}

// Answer is the person's answer to an Ask.
type Answer struct {
	// AskID is the ClientEventID of the event that asked.
	AskID string
	// Questions is one answer per question, in order (AskKindQuestion).
	Questions []QuestionAnswer
	// Feedback is what to change, so the agent keeps planning (AskKindPlan).
	Feedback string
}

// QuestionAnswer is the labels picked, or what the person typed instead.
type QuestionAnswer struct {
	Labels []string
	Text   string
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

// AnswerServer is a provider whose in-session script waits, beside the
// agent's own dialog, for the person's answer from Room to a question.
type AnswerServer interface {
	// CanAnswer records that the session's script takes answers from Room,
	// so the questions it asks are offered there.
	CanAnswer(providerSessionID string)
	// WaitAnswer holds the script's request (the provider's own JSON) until
	// the question it names has an answer, closes, or a while passes. The
	// reply goes back to the script verbatim.
	WaitAnswer(ctx context.Context, body []byte) ([]byte, error)
}

// Limits Room's API enforces. The uplink holds every provider to them, so one
// oversized row can never stall the queue.
const (
	MaxEventText = 100_000
	MaxTitle     = 200
	MaxCwd       = 4096
	MaxToolName  = 128
	MaxLabel     = 128
)

// Limits on an Ask, which room-be refuses beyond: a provider leaves out an
// Ask that breaks one (FitsRoom) rather than lose the whole event.
const (
	MaxAskQuestions   = 8
	MaxAskOptions     = 8
	MaxAskQuestion    = 2000
	MaxAskHeader      = 64
	MaxAskLabel       = 200
	MaxAskDescription = 1000
)

// FitsRoom reports whether room-be accepts the ask as it is: within the
// limits, a plan without questions, questions with options, and no question
// or option label repeated (an answer names them).
func (a *Ask) FitsRoom() bool {
	switch a.Kind {
	case AskKindPlan:
		return len(a.Questions) == 0
	case AskKindQuestion:
	default:
		return false
	}
	if len(a.Questions) == 0 || len(a.Questions) > MaxAskQuestions {
		return false
	}
	questions := map[string]bool{}
	for _, q := range a.Questions {
		if q.Question == "" || len(q.Question) > MaxAskQuestion || len(q.Header) > MaxAskHeader ||
			questions[q.Question] || len(q.Options) == 0 || len(q.Options) > MaxAskOptions {
			return false
		}
		questions[q.Question] = true
		labels := map[string]bool{}
		for _, o := range q.Options {
			if o.Label == "" || len(o.Label) > MaxAskLabel || len(o.Description) > MaxAskDescription || labels[o.Label] {
				return false
			}
			labels[o.Label] = true
		}
	}
	return true
}

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
