package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// A turn waits on the person at a permission prompt, a question
// (AskUserQuestion, ExitPlanMode), an MCP elicitation or a dialog. Each has a
// hook as it opens, but answering one mostly has none: a granted tool ends in
// its PostToolUse, a denied one fires nothing and Claude just goes on. So a
// wait ends at the waited tool's PostToolUse, an ElicitationResult, or the
// first sign of Claude moving on: a tool call or a reply it had not started
// before. These hooks all run in the background and can arrive late, so a
// late PreToolUse of the waited tool itself does not end the wait. `claude
// agents` (reconcile) corrects what the hooks miss.

// asksPerson are the tools whose call is itself a question to the person.
var asksPerson = map[string]bool{"AskUserQuestion": true, "ExitPlanMode": true}

// needsPerson are the Notification types that mean the session waits on the
// person; idle_prompt, a minute after a finished turn, is not one.
var needsPerson = map[string]bool{"permission_prompt": true, "elicitation_dialog": true, "agent_needs_input": true}

// onToolStart records a PreToolUse and reads what it says about a wait.
func (p *Provider) onToolStart(s *session, in hookInput, now time.Time) {
	isNew := in.ToolUseID != "" && s.startedTool(in.ToolUseID) < 0
	if asksPerson[in.ToolName] {
		fp := fingerprint(in)
		if isNew {
			// Answering a question changes its input: keep it by id.
			s.tools = remember(s.tools, toolCall{id: in.ToolUseID, fingerprint: fp})
		}
		p.wait(s, fp, in.ToolName, now)
		return
	}
	if !isNew {
		return
	}
	s.tools = remember(s.tools, toolCall{id: in.ToolUseID})
	if s.info.State == core.StateNeedsInput && fingerprint(in) != s.waitTool {
		// A call the wait did not know of: the person answered, or turned
		// the tool down and Claude went on.
		p.resume(s, now)
	}
}

// onToolEnd ends a wait on the tool that just finished, matched by its input
// or, for a question, by the id its PreToolUse had.
func (p *Provider) onToolEnd(s *session, in hookInput, now time.Time) {
	if s.info.State != core.StateNeedsInput || s.waitTool == "" {
		return
	}
	if i := s.startedTool(in.ToolUseID); (i >= 0 && s.tools[i].fingerprint == s.waitTool) || fingerprint(in) == s.waitTool {
		p.resume(s, now)
	}
}

// startedTool returns the index in s.tools of a call the turn started, or -1.
func (s *session) startedTool(id string) int {
	return slices.IndexFunc(s.tools, func(t toolCall) bool { return t.id == id })
}

// wait marks the session blocked on the person. tool is the fingerprint of
// the tool being asked about and toolName its name, both "" if there is none
// (an elicitation, a dialog). The name rides on the wait's event, so the app
// can say what the person is asked for.
func (p *Provider) wait(s *session, tool, toolName string, now time.Time) {
	if s.stopped || s.info.State == core.StateSleeping {
		return // between turns, or a late hook of a finished one
	}
	if s.info.State == core.StateNeedsInput {
		if tool != "" {
			s.waitTool = tool // the newest prompt is the one on screen
		}
		return
	}
	s.waitTool = tool
	p.changeState(s, core.StateNeedsInput, toolName, now)
}

// resume marks a session that waited on the person working again.
func (p *Provider) resume(s *session, now time.Time) {
	if !s.stopped && s.info.State == core.StateNeedsInput {
		p.changeState(s, core.StateWorking, "", now)
	}
}

// changeState reports a wait or its end under an id of its own: one prompt
// can wait and resume many times. A wait names the tool it asks about, when
// it knows one.
func (p *Provider) changeState(s *session, state core.State, toolName string, now time.Time) {
	prefix := "work:"
	if state == core.StateNeedsInput {
		prefix = "wait:"
	}
	s.changes++
	s.info.State = state
	p.emit(s, core.Event{
		ClientEventID: prefix + s.promptID + ":" + strconv.Itoa(s.changes),
		Kind:          core.KindStateChange,
		State:         state,
		ToolName:      toolName,
	}, now)
}

// fingerprint identifies a tool call by its name and input, so the
// PermissionRequest, PreToolUse and PostToolUse of one call match. It is
// hashed to keep a large input (a file being written) out of memory.
func fingerprint(in hookInput) string {
	// It was decoded from JSON, so it encodes; map keys come out sorted.
	input, _ := json.Marshal(in.ToolInput)
	sum := sha256.Sum256(append([]byte(in.ToolName+"\x00"), input...))
	return hex.EncodeToString(sum[:])
}
