package claudecode

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// asks is an AskUserQuestion's input as decoded from the hook's JSON.
func asks(questions ...map[string]any) map[string]any {
	qs := make([]any, len(questions))
	for i, q := range questions {
		qs[i] = q
	}
	return map[string]any{"questions": qs}
}

func question(text string, multi bool, options ...map[string]any) map[string]any {
	opts := make([]any, len(options))
	for i, o := range options {
		opts[i] = o
	}
	return map[string]any{"question": text, "header": "Chip", "multiSelect": multi, "options": opts}
}

func option(label, desc string) map[string]any {
	return map[string]any{"label": label, "description": desc, "preview": "┌──┐"}
}

func TestDescribeAsk(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{"one question", "AskUserQuestion", asks(question("Which db?", false, option("Postgres", "Relational"), option("SQLite", ""))),
			"Which db?\n\n- **Postgres** — Relational\n- **SQLite**"},
		{"multi-select", "AskUserQuestion", asks(question("Which checks?", true, option("Lint", "Fast"))),
			"Which checks? (choose any)\n\n- **Lint** — Fast"},
		{"two questions", "AskUserQuestion", asks(question("A?", false, option("x", "")), question("B?", false, option("y", ""))),
			"A?\n\n- **x**\n\nB?\n\n- **y**"},
		{"options kept on one line", "AskUserQuestion", asks(question("  Pick  ", false, option("Two\nlines", "a\n  b"))),
			"Pick\n\n- **Two lines** — a b"},
		{"no options", "AskUserQuestion", asks(question("Anything else?", false)), "Anything else?"},
		{"malformed", "AskUserQuestion", map[string]any{"questions": []any{"oops", map[string]any{"question": 3}}}, ""},
		{"no questions", "AskUserQuestion", map[string]any{}, ""},
		{"plan", "ExitPlanMode", map[string]any{"plan": "\n# Plan\n\n1. Do it\n"}, "# Plan\n\n1. Do it"},
		{"no plan", "ExitPlanMode", map[string]any{}, ""},
		{"another tool", "Bash", map[string]any{"plan": "x"}, ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, describeAsk(tc.tool, tc.input), tc.name)
	}
}
