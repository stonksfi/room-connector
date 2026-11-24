package claudecode

import (
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// ask reports a question to the person (asksPerson; fp its fingerprint) as
// the agent's message, once: its PreToolUse and PermissionRequest both carry
// it, and either can come first. It is not the turn's reply, so the Stop's
// reply is still due.
func (p *Provider) ask(s *session, in hookInput, fp string, now time.Time) {
	id := "ask:" + s.promptID + ":" + fp[:16]
	if s.isClosed(id) {
		return
	}
	text := describeAsk(in.ToolName, in.ToolInput)
	if text == "" {
		return
	}
	s.close(id)
	p.emit(s, core.Event{ClientEventID: id, Kind: core.KindAssistantMessage, Text: text}, now)
}

// describeAsk turns a question to the person into the markdown the app shows:
// an AskUserQuestion's questions with their options, or an ExitPlanMode's
// plan. It returns "" when the input holds nothing to show.
func describeAsk(tool string, input map[string]any) string {
	switch tool {
	case "ExitPlanMode":
		plan, _ := input["plan"].(string)
		return strings.TrimSpace(plan)
	case "AskUserQuestion":
		questions, _ := input["questions"].([]any)
		var blocks []string
		for _, q := range questions {
			if b := describeQuestion(q); b != "" {
				blocks = append(blocks, b)
			}
		}
		return strings.Join(blocks, "\n\n")
	}
	return ""
}

// describeQuestion is one AskUserQuestion question and its options as a list.
// Its header and the options' previews are left out.
func describeQuestion(q any) string {
	m, _ := q.(map[string]any)
	text, _ := m["question"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if multi, _ := m["multiSelect"].(bool); multi {
		text += " (choose any)"
	}
	options, _ := m["options"].([]any)
	var items []string
	for _, o := range options {
		if item := describeOption(o); item != "" {
			items = append(items, item)
		}
	}
	if len(items) > 0 {
		text += "\n\n" + strings.Join(items, "\n")
	}
	return text
}

func describeOption(o any) string {
	m, _ := o.(map[string]any)
	label, _ := m["label"].(string)
	desc, _ := m["description"].(string)
	// One line each: a newline would end the list item.
	label, desc = oneLine(label), oneLine(desc)
	if label == "" {
		return ""
	}
	if desc == "" {
		return "- **" + label + "**"
	}
	return "- **" + label + "** — " + desc
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
