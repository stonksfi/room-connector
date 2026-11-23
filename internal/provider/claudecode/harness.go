package claudecode

import (
	"regexp"
	"strings"
	"unicode"
)

// Claude Code's harness hands the agent some things as a prompt of its own,
// the whole prompt wrapped in one tag: a subagent's final report
// (<agent-message>), a background task finishing (<task-notification>). They
// fire UserPromptSubmit like the person's prompts but are not the person's
// words, so they are reported as notes.

var (
	harnessOpen = regexp.MustCompile(`^<([a-z][a-z0-9]*(?:-[a-z0-9]+)*)(?:\s[^>]*)?>`)
	// What the harness tells the agent before a subagent's report.
	handbackPreamble = regexp.MustCompile(`^\s*\[Subagent hand-back\][\s\S]*?The report follows:[^\S\n]*\n`)
	taskSummary      = regexp.MustCompile(`<summary>([\s\S]*?)</summary>`)
	anyTag           = regexp.MustCompile(`</?[a-z][a-z0-9-]*>`)
)

// harnessTag is one tag the harness wraps a prompt in: the note's label, and
// how to read its body.
type harnessTag struct {
	label string
	read  func(body string) string
}

var harnessTags = map[string]harnessTag{
	"agent-message": {
		label: "Subagent report",
		read:  func(body string) string { return handbackPreamble.ReplaceAllString(body, "") },
	},
	"task-notification": {
		label: "Background task",
		// The rest is ids and file paths for the agent; the summary is the news.
		read: func(body string) string {
			if m := taskSummary.FindStringSubmatch(body); m != nil {
				return m[1]
			}
			return anyTag.ReplaceAllString(body, "")
		},
	},
}

// readHarness reports whether a prompt was written by Claude Code's harness,
// and if so the note's label and text: the body without its tag, a report's
// preamble, or the indent the harness adds to every line. A report cut off
// before its closing tag still reads; a prompt that goes on after the closing
// tag, or is wrapped in any other tag, is the person's.
func readHarness(prompt string) (label, text string, ok bool) {
	trimmed := strings.TrimSpace(prompt)
	m := harnessOpen.FindStringSubmatch(trimmed)
	if m == nil {
		return "", "", false
	}
	tag, known := harnessTags[m[1]]
	if !known {
		return "", "", false
	}
	body := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(trimmed[len(m[0]):])
	closing := "</" + m[1] + ">"
	if cut, closed := strings.CutSuffix(body, closing); closed {
		body = cut
	} else if strings.Contains(body, closing) {
		return "", "", false
	}
	return tag.label, dedent(tag.read(body)), true
}

// dedent removes the indentation every non-blank line shares, and blank lines
// at either end.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	indent := func(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }
	shared := -1
	for _, line := range lines {
		if strings.TrimSpace(line) != "" && (shared < 0 || indent(line) < shared) {
			shared = indent(line)
		}
	}
	for i, line := range lines {
		lines[i] = line[min(max(shared, 0), indent(line)):]
	}
	return strings.TrimRightFunc(strings.TrimLeft(strings.Join(lines, "\n"), "\n"), unicode.IsSpace)
}
