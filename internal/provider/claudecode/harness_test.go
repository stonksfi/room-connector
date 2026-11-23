package claudecode

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A subagent's report as Claude Code hands it back (preamble shortened).
var handback = strings.Join([]string{
	`<agent-message from="a738f0518741bbcfe">`,
	"[Subagent hand-back] The text below is the final report of a subagent this session delegated to. The harness indents every line of the report. The report follows:",
	"  ## Code review: Room widget diff (room-fe)",
	"",
	"  No BLOCKERs.",
	"",
	"  - **W1** token is never refreshed",
	"    - nested detail",
	"</agent-message>",
}, "\n")

func TestReadHarness_SubagentReport(t *testing.T) {
	label, text, ok := readHarness(handback)
	assert.True(t, ok)
	assert.Equal(t, "Subagent report", label)
	assert.Equal(t, "## Code review: Room widget diff (room-fe)\n\nNo BLOCKERs.\n\n- **W1** token is never refreshed\n  - nested detail", text,
		"the preamble and the harness's indent are gone; nesting is kept")
}

func TestReadHarness_ReportCutOffBeforeItsClosingTag(t *testing.T) {
	label, text, ok := readHarness("<agent-message from=\"x\">\r\n[Subagent hand-back] The report follows:\r\n  Partial")
	assert.True(t, ok)
	assert.Equal(t, "Subagent report", label)
	assert.Equal(t, "Partial", text)
}

func TestReadHarness_BackgroundTaskIsItsSummary(t *testing.T) {
	task := strings.Join([]string{
		"<task-notification>",
		"<task-id>bnla6z3q4</task-id>",
		"<status>failed</status>",
		`<summary>Background command "Start Metro" failed with exit code 144</summary>`,
		"</task-notification>",
	}, "\n")
	label, text, ok := readHarness(task)
	assert.True(t, ok)
	assert.Equal(t, "Background task", label)
	assert.Equal(t, `Background command "Start Metro" failed with exit code 144`, text)

	_, text, _ = readHarness("<task-notification>\n<task-id>b1</task-id>\n<status>done</status>\n</task-notification>")
	assert.Equal(t, "b1\ndone", text, "without a summary, the tags come off")
}

func TestReadHarness_LeavesThePersonsPromptsAlone(t *testing.T) {
	for _, prompt := range []string{
		"fix the bug",
		"please fix <b>this</b>",
		"<details>my own notes</details>",
		"<div> is broken on Android",
		"<Button> renders twice",
		"<agent-message>x</agent-message> and then do this",
	} {
		_, _, ok := readHarness(prompt)
		assert.False(t, ok, prompt)
	}
}

func TestDedent(t *testing.T) {
	assert.Equal(t, "a\n  b", dedent("\n\n  a\n    b\n  \n"))
	assert.Equal(t, "", dedent("\n  \n"))
}
