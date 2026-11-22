package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// hookInput is the subset of Claude Code's hook JSON the provider reads.
// See https://code.claude.com/docs/en/hooks.
type hookInput struct {
	HookEventName        string         `json:"hook_event_name"`
	SessionID            string         `json:"session_id"`
	Cwd                  string         `json:"cwd"`
	PromptID             string         `json:"prompt_id"`
	SessionTitle         string         `json:"session_title"`
	Source               string         `json:"source"`
	Prompt               string         `json:"prompt"`
	ToolName             string         `json:"tool_name"`
	ToolInput            map[string]any `json:"tool_input"`
	ToolUseID            string         `json:"tool_use_id"`
	MessageID            string         `json:"message_id"`
	Index                int            `json:"index"`
	Final                bool           `json:"final"`
	Delta                string         `json:"delta"`
	LastAssistantMessage string         `json:"last_assistant_message"`
	StopHookActive       bool           `json:"stop_hook_active"`
}

// stopBlock is the Stop hook answer that makes Claude continue with a message.
type stopBlock struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// maxToolSummary bounds a tool row's text: the app shows a line, not a payload.
const maxToolSummary = 300

// HandleHook maps one hook call to Room events. Only Stop answers anything:
// when a message is waiting and no in-session script is polling, the answer
// hands the message to Claude so it continues with it.
func (p *Provider) HandleHook(ctx context.Context, body []byte) ([]byte, error) {
	var in hookInput
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode hook input: %w", err)
	}
	if in.SessionID == "" {
		return nil, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sink == nil {
		return nil, nil // not started yet
	}
	now := p.now()
	s := p.get(in.SessionID)
	s.lastHookAt = now
	s.info.LastActivityAt = now
	if in.Cwd != "" {
		s.info.Cwd = in.Cwd
	}
	if in.SessionTitle != "" {
		s.info.Title = in.SessionTitle
	}
	if s.info.Title == "" {
		s.info.Title = titleFor("", s.info.Cwd)
	}

	switch in.HookEventName {
	case "SessionStart":
		// Compaction restarts the session mid-turn; only a real start is idle.
		if in.Source != "compact" {
			s.info.State = core.StateSleeping
		}
		p.sink.UpsertSession(s.info)
	case "UserPromptSubmit":
		p.onPrompt(s, in, now)
	case "PreToolUse":
		p.emit(s, core.Event{
			ClientEventID: "tool:" + in.ToolUseID,
			Kind:          core.KindToolActivity,
			ToolName:      in.ToolName,
			Text:          summarizeTool(in.ToolName, in.ToolInput),
		}, now)
	case "MessageDisplay":
		p.onDisplay(s, in, now)
	case "Stop":
		return p.onStop(ctx, s, in, now, true)
	case "StopFailure":
		// Its hook runs in the background, so an answer would be ignored:
		// never hand a message over here.
		return p.onStop(ctx, s, in, now, false)
	case "SessionEnd":
		p.setState(s, core.StateEnded, "end:"+strconv.FormatInt(now.UnixNano(), 10), now)
		p.forget(in.SessionID)
	}
	return nil, nil
}

func (p *Provider) onPrompt(s *session, in hookInput, now time.Time) {
	s.promptID = in.PromptID
	s.stops = 0
	s.assistantSent = false
	s.stopped = false
	ev := core.Event{
		ClientEventID: "prompt:" + in.PromptID,
		Kind:          core.KindUserMessage,
		Text:          in.Prompt,
	}
	// A prompt the in-session script submitted is the app's message landing.
	for i, q := range s.handed {
		if !q.viaStop && strings.TrimSpace(q.cmd.Text) == strings.TrimSpace(in.Prompt) {
			ev.CommandID = q.cmd.ID
			s.handed = append(s.handed[:i], s.handed[i+1:]...)
			p.sink.Settle(q.cmd.ID, nil)
			break
		}
	}
	p.emit(s, ev, now)
	p.setState(s, core.StateWorking, "work:"+in.PromptID, now)
}

func (p *Provider) onDisplay(s *session, in hookInput, now time.Time) {
	if s.isClosed(in.MessageID) || s.stopped {
		return // a late chunk of a reply already reported at Stop
	}
	d, ok := s.drafts[in.MessageID]
	if !ok {
		d = newDraft()
		s.drafts[in.MessageID] = d
	}
	text, done := d.add(in.Index, in.Delta, in.Final)
	if !done {
		return
	}
	delete(s.drafts, in.MessageID)
	s.close(in.MessageID)
	p.emitAssistant(s, "msg:"+in.MessageID, text, now)
}

func (p *Provider) onStop(ctx context.Context, s *session, in hookInput, now time.Time, canBlock bool) ([]byte, error) {
	s.stops++
	p.finishReply(s, in.LastAssistantMessage, now)

	if in.StopHookActive {
		// Claude continued after a Stop answer and finished: what it was
		// handed is delivered.
		p.confirmViaStop(s)
	}

	if canBlock && ctx.Err() == nil && len(s.inbox) > 0 && now.Sub(s.lastPollAt) > pollFresh {
		// No in-session script is polling (mods off): continue the turn with
		// the message instead. It never shows up as a prompt, so it is
		// reported here and delivered once Claude is seen continuing.
		q := s.inbox[0]
		s.inbox = s.inbox[1:]
		q.viaStop, q.at = true, now
		s.handed = append(s.handed, q)
		p.emit(s, core.Event{
			ClientEventID: "cmd:" + strconv.FormatInt(q.cmd.ID, 10),
			Kind:          core.KindUserMessage,
			Text:          q.cmd.Text,
			CommandID:     q.cmd.ID,
		}, now)
		return json.Marshal(stopBlock{
			Decision: "block",
			Reason:   "The person sent this message from the Room app on their phone. Treat it as their next prompt:\n\n" + q.cmd.Text,
		})
	}
	s.stopped = true
	p.setState(s, core.StateSleeping, "sleep:"+s.promptID+":"+strconv.Itoa(s.stops), now)
	return nil, nil
}

// finishReply reports the reply that ended at this Stop exactly once. Chunks
// run in the background and can trail their Stop, so a reply still in
// progress is reported with the Stop's full text and its id closed.
func (p *Provider) finishReply(s *session, last string, now time.Time) {
	for id, d := range s.drafts {
		text := d.text()
		if len(s.drafts) == 1 && last != "" {
			text = last
		}
		s.close(id)
		p.emitAssistant(s, "msg:"+id, text, now)
	}
	s.drafts = map[string]*draft{}
	if !s.assistantSent {
		p.emitAssistant(s, "stop:"+s.promptID+":"+strconv.Itoa(s.stops), last, now)
	}
	s.assistantSent = false // a continued turn has a reply of its own
}

// confirmViaStop settles the messages handed over in Stop answers.
func (p *Provider) confirmViaStop(s *session) {
	kept := s.handed[:0]
	for _, q := range s.handed {
		if q.viaStop {
			p.sink.Settle(q.cmd.ID, nil)
			continue
		}
		kept = append(kept, q)
	}
	s.handed = kept
}

func (p *Provider) emitAssistant(s *session, clientID, text string, now time.Time) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.assistantSent = true
	p.emit(s, core.Event{ClientEventID: clientID, Kind: core.KindAssistantMessage, Text: text}, now)
}

func (p *Provider) setState(s *session, state core.State, clientID string, now time.Time) {
	s.info.State = state
	p.emit(s, core.Event{ClientEventID: clientID, Kind: core.KindStateChange, State: state}, now)
}

// emit fills the event's session fields and reports it; the caller holds p.mu.
func (p *Provider) emit(s *session, e core.Event, now time.Time) {
	e.Provider = Name
	e.ProviderSessionID = s.info.ProviderSessionID
	e.OccurredAt = now
	p.sink.Emit(e)
}

// summarizeTool turns a tool call into the one line the app shows.
func summarizeTool(tool string, input map[string]any) string {
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := input[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	var s string
	switch tool {
	case "Bash":
		s = pick("command")
	case "Read", "Edit", "Write", "NotebookEdit":
		s = pick("file_path", "notebook_path")
	case "Grep", "Glob":
		s = pick("pattern")
	case "WebFetch":
		s = pick("url")
	case "WebSearch":
		s = pick("query")
	case "Task", "Agent":
		s = pick("description", "prompt")
	default:
		s = pick("description", "command", "file_path", "query", "url", "pattern")
	}
	return core.Truncate(strings.Join(strings.Fields(s), " "), maxToolSummary)
}
