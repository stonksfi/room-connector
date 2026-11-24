package claudecode

import (
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// ask reports a question to the person (asksPerson; fp its fingerprint) as
// the agent's message, once: its PreToolUse and PermissionRequest both carry
// it, and either can come first. It is not the turn's reply, so the Stop's
// reply is still due. It carries the structured Ask when Room can answer it:
// the session's script takes answers, and the question is the main loop's.
func (p *Provider) ask(s *session, in hookInput, fp string, now time.Time) {
	prompt := in.PromptID
	if prompt == "" {
		prompt = s.promptID
	}
	id := "ask:" + prompt + ":" + printOf(fp)
	if s.isClosed(id) {
		return
	}
	text := describeAsk(in.ToolName, in.ToolInput)
	if text == "" {
		return
	}
	s.close(id)
	e := core.Event{ClientEventID: id, Kind: core.KindAssistantMessage, Text: text}
	if in.AgentID == "" && s.takesAnswers && now.Sub(s.lastPollAt) <= pollFresh {
		e.Ask = buildAsk(in.ToolName, in.ToolInput)
	}
	p.emit(s, e, now)
}

// buildAsk is the structured form of a question to the person, or nil when
// Room cannot answer it: a question that is not a plain choice, or one
// room-be would refuse.
func buildAsk(tool string, input map[string]any) *core.Ask {
	if tool == "ExitPlanMode" {
		return &core.Ask{Kind: core.AskKindPlan}
	}
	questions, choices := readQuestions(input)
	a := &core.Ask{Kind: core.AskKindQuestion, Questions: questions}
	if !choices || !a.FitsRoom() {
		return nil
	}
	return a
}

// readQuestions decodes an AskUserQuestion's questions as asked, one per
// entry (a malformed one left empty), and whether every one is a plain
// choice: Claude Code also asks for text and numbers. The question text is
// kept exactly: it is the key Claude Code reads the answer by.
func readQuestions(input map[string]any) (questions []core.AskQuestion, choices bool) {
	raw, _ := input["questions"].([]any)
	choices = true
	for _, q := range raw {
		m, _ := q.(map[string]any)
		if kind, _ := m["kind"].(string); kind != "" && kind != "choice" {
			choices = false
		}
		text, _ := m["question"].(string)
		header, _ := m["header"].(string)
		multi, _ := m["multiSelect"].(bool)
		aq := core.AskQuestion{Question: text, Header: header, MultiSelect: multi}
		options, _ := m["options"].([]any)
		for _, o := range options {
			om, _ := o.(map[string]any)
			label, _ := om["label"].(string)
			desc, _ := om["description"].(string)
			aq.Options = append(aq.Options, core.AskOption{Label: label, Description: desc})
		}
		questions = append(questions, aq)
	}
	return questions, choices
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
		questions, _ := readQuestions(input)
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

// describeQuestion is one question and its options as a list. Its header
// and the options' previews are left out.
func describeQuestion(q core.AskQuestion) string {
	text := strings.TrimSpace(q.Question)
	if text == "" {
		return ""
	}
	if q.MultiSelect {
		text += " (choose any)"
	}
	var items []string
	for _, o := range q.Options {
		if item := describeOption(o); item != "" {
			items = append(items, item)
		}
	}
	if len(items) > 0 {
		text += "\n\n" + strings.Join(items, "\n")
	}
	return text
}

func describeOption(o core.AskOption) string {
	// One line each: a newline would end the list item.
	label, desc := oneLine(o.Label), oneLine(o.Description)
	if label == "" {
		return ""
	}
	if desc == "" {
		return "- **" + label + "**"
	}
	return "- **" + label + "** — " + desc
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
