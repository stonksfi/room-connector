package claudecode

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stonksfi/room-connector/internal/core"
)

var (
	dbQuestion = asks(question("Which db?", false, option("Postgres", "Relational"), option("SQLite", "")))
	dbAnswered = map[string]any{"questions": dbQuestion["questions"], "answers": map[string]any{"Which db?": "Postgres"}}
	thePlan    = map[string]any{"plan": "# Plan\n\n1. Do it", "planFilePath": "/p.md"}
)

// polledTakingAnswers is the script's inbox poll with caps=answer.
func polledTakingAnswers(p *Provider) {
	p.CanAnswer("s1")
	p.TakeInbox("s1")
}

// asking starts turn p1 in a session whose script takes answers and opens
// the question tool's dialog (PreToolUse, then PermissionRequest).
func asking(t *testing.T, p *Provider, tool string, input map[string]any) {
	t.Helper()
	polledTakingAnswers(p)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_use_id": "t1", "tool_input": input})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": tool, "tool_input": input})
}

// waitNow runs the script's wait for the question and returns its reply.
func waitNow(t *testing.T, p *Provider, tool string, input map[string]any) waitReply {
	body, err := json.Marshal(waitRequest{Session: "s1", ToolName: tool, ToolInput: input})
	assert.NoError(t, err)
	raw, err := p.WaitAnswer(context.Background(), body)
	assert.NoError(t, err)
	var r waitReply
	assert.NoError(t, json.Unmarshal(raw, &r))
	return r
}

// waitFor runs the script's wait for the question in the background.
func waitFor(t *testing.T, p *Provider, tool string, input map[string]any) <-chan waitReply {
	t.Helper()
	out := make(chan waitReply, 1)
	go func() { out <- waitNow(t, p, tool, input) }()
	print := printFor(tool, input)
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		w := p.sessions["s1"].waits[print]
		return w != nil && w.open > 0
	}, time.Second, time.Millisecond, "the wait is registered")
	return out
}

// markWaitSeen records that a wait for the question just ended.
func markWaitSeen(p *Provider, tool string, input map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions["s1"].waits[printFor(tool, input)] = &waitState{seen: p.now()}
}

func answerCmd(id int64, tool string, input map[string]any, a core.Answer) core.Command {
	a.AskID = askID(tool, input)
	return core.Command{ID: id, Kind: core.CommandAnswer, Text: "summary", Answer: &a}
}

func receive(t *testing.T, ch <-chan waitReply) waitReply {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not answer")
		return waitReply{}
	}
}

func TestAnswer_QuestionAnsweredFromRoom(t *testing.T) {
	p, r, _ := newTestProvider(t)
	asking(t, p, "AskUserQuestion", dbQuestion)
	wait := waitFor(t, p, "AskUserQuestion", dbQuestion)

	pick := answerCmd(7, "AskUserQuestion", dbQuestion, core.Answer{Questions: []core.QuestionAnswer{{Labels: []string{"Postgres"}}}})
	require.NoError(t, p.Deliver(context.Background(), "s1", pick))

	reply := receive(t, wait)
	assert.Equal(t, "allow", reply.Decision["behavior"])
	updated := reply.Decision["updatedInput"].(map[string]any)
	assert.Equal(t, map[string]any{"Which db?": "Postgres"}, updated["answers"])
	assert.Len(t, updated["questions"], 1, "the call's own input, with the answers added")

	// The tool ran with Room's answer: it is delivered and shows as the person's.
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbAnswered})
	settled, ok := r.settled[7]
	assert.True(t, ok)
	assert.NoError(t, settled)
	last := r.events[len(r.events)-2] // the answer, then the work that follows
	assert.Equal(t, core.Event{Provider: Name, ProviderSessionID: "s1", ClientEventID: "answer:7", Kind: core.KindUserMessage,
		Text: "summary", CommandID: 7, OccurredAt: last.OccurredAt}, last)
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func TestAnswer_TheMacAnsweredFirst(t *testing.T) {
	p, r, _ := newTestProvider(t)
	asking(t, p, "AskUserQuestion", dbQuestion)
	wait := waitFor(t, p, "AskUserQuestion", dbQuestion)
	pick := answerCmd(7, "AskUserQuestion", dbQuestion, core.Answer{Questions: []core.QuestionAnswer{{Labels: []string{"SQLite"}}}})
	require.NoError(t, p.Deliver(context.Background(), "s1", pick))
	receive(t, wait)

	// The person picked at the Mac at the same moment: Claude got their answer.
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbAnswered})
	assert.ErrorIs(t, r.settled[7], errAnsweredOnMac)

	// The question is closed: a new wait for it ends at once.
	assert.True(t, waitNow(t, p, "AskUserQuestion", dbQuestion).Closed)
}

func TestAnswer_AnsweredAtTheMacWhileQueued(t *testing.T) {
	p, r, _ := newTestProvider(t)
	asking(t, p, "AskUserQuestion", dbQuestion)
	markWaitSeen(p, "AskUserQuestion", dbQuestion)
	pick := answerCmd(7, "AskUserQuestion", dbQuestion, core.Answer{Questions: []core.QuestionAnswer{{Text: "Mongo"}}})
	require.NoError(t, p.Deliver(context.Background(), "s1", pick), "a wait just ran: it is still answerable")

	// Answered at the Mac before the next wait took it.
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbAnswered})
	assert.ErrorIs(t, r.settled[7], errAnsweredOnMac)
}

func TestAnswer_RefusedUnlessTheQuestionIsOpenWithAWait(t *testing.T) {
	p, _, _ := newTestProvider(t)
	pick := func(id int64) core.Command {
		return answerCmd(id, "AskUserQuestion", dbQuestion, core.Answer{Questions: []core.QuestionAnswer{{Labels: []string{"Postgres"}}}})
	}
	assert.ErrorIs(t, p.Deliver(context.Background(), "s1", pick(1)), errNotOpen, "nothing asked")

	asking(t, p, "AskUserQuestion", dbQuestion)
	assert.ErrorIs(t, p.Deliver(context.Background(), "s1", pick(2)), errNoWait, "no script waits on it")

	wait := waitFor(t, p, "AskUserQuestion", dbQuestion)
	other := answerCmd(3, "AskUserQuestion", asks(question("Other?", false, option("x", ""))), core.Answer{})
	assert.ErrorIs(t, p.Deliver(context.Background(), "s1", other), errNotOpen, "a question not on screen")
	require.NoError(t, p.Deliver(context.Background(), "s1", pick(4)))
	receive(t, wait)
	assert.ErrorIs(t, p.Deliver(context.Background(), "s1", pick(5)), errAnswered)
}

func TestAnswer_PlanFeedbackKeepsPlanning(t *testing.T) {
	p, r, _ := newTestProvider(t)
	asking(t, p, "ExitPlanMode", thePlan)
	wait := waitFor(t, p, "ExitPlanMode", thePlan)
	keep := answerCmd(8, "ExitPlanMode", thePlan, core.Answer{Feedback: "  split step 2 "})
	require.NoError(t, p.Deliver(context.Background(), "s1", keep))

	reply := receive(t, wait)
	assert.Equal(t, "deny", reply.Decision["behavior"])
	assert.Contains(t, reply.Decision["message"], "Room app")
	assert.Contains(t, reply.Decision["message"], "\n\nsplit step 2")

	// Refused, the plan runs no PostToolUse: Claude goes on planning.
	_, settled := r.settled[8]
	assert.False(t, settled)
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_use_id": "t2", "tool_input": map[string]any{"file_path": "/a"}})
	assert.NoError(t, r.settled[8])
	assert.Contains(t, r.kinds(), core.KindUserMessage)
}

func TestAnswer_PlanApprovedAtTheMac(t *testing.T) {
	p, r, _ := newTestProvider(t)
	asking(t, p, "ExitPlanMode", thePlan)
	wait := waitFor(t, p, "ExitPlanMode", thePlan)
	require.NoError(t, p.Deliver(context.Background(), "s1", answerCmd(8, "ExitPlanMode", thePlan, core.Answer{Feedback: "more"})))
	receive(t, wait)
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "tool_use_id": "t1", "tool_input": thePlan})
	assert.ErrorIs(t, r.settled[8], errApprovedOnMac)
}

func TestAnswer_AnAnswerThatDoesNotFitFails(t *testing.T) {
	p, r, _ := newTestProvider(t)
	asking(t, p, "AskUserQuestion", dbQuestion)
	wait := waitFor(t, p, "AskUserQuestion", dbQuestion)
	two := answerCmd(9, "AskUserQuestion", dbQuestion, core.Answer{Questions: []core.QuestionAnswer{{Text: "a"}, {Text: "b"}}})
	require.NoError(t, p.Deliver(context.Background(), "s1", two))
	assert.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return r.settled[9] != nil
	}, time.Second, time.Millisecond)
	assert.ErrorIs(t, r.settled[9], errAnswerShape)

	// The dialog stays the person's; the wait goes on until the question closes.
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbAnswered})
	assert.True(t, receive(t, wait).Closed)
}

func TestAnswer_ALateHookOfAnAnsweredQuestionDoesNotWaitAgain(t *testing.T) {
	p, r, _ := newTestProvider(t)
	asking(t, p, "AskUserQuestion", dbQuestion)
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbAnswered})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "AskUserQuestion", "tool_input": dbQuestion})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}

func TestAnswer_WaitHoldsThenAnswersNothing(t *testing.T) {
	p, _, _ := newTestProvider(t)
	p.answerHold = 20 * time.Millisecond
	asking(t, p, "AskUserQuestion", dbQuestion)
	assert.Equal(t, waitReply{}, waitNow(t, p, "AskUserQuestion", dbQuestion), "nothing yet: the script asks again")

	_, err := p.WaitAnswer(context.Background(), []byte(`{"session":"s1","tool_name":"Bash"}`))
	assert.Error(t, err, "only question tools wait")
}

func TestAnswer_ExpiryAndEnd(t *testing.T) {
	p, r, c := newTestProvider(t)
	asking(t, p, "AskUserQuestion", dbQuestion)
	markWaitSeen(p, "AskUserQuestion", dbQuestion)
	require.NoError(t, p.Deliver(context.Background(), "s1", answerCmd(1, "AskUserQuestion", dbQuestion, core.Answer{Questions: []core.QuestionAnswer{{Text: "x"}}})))

	c.advance(untakenAnswerTimeout + time.Second)
	p.expire()
	assert.ErrorIs(t, r.settled[1], errNoWait, "no wait came to take it")

	wait := waitFor(t, p, "AskUserQuestion", dbQuestion)
	require.NoError(t, p.Deliver(context.Background(), "s1", answerCmd(2, "AskUserQuestion", dbQuestion, core.Answer{Questions: []core.QuestionAnswer{{Text: "y"}}})))
	receive(t, wait)
	hook(t, p, map[string]any{"hook_event_name": "SessionEnd"})
	assert.ErrorIs(t, r.settled[2], errEnded)
}

func TestAsk_IsAnswerableOnlyWhereRoomCanAnswer(t *testing.T) {
	cases := []struct {
		name       string
		canAnswer  bool
		agent      string
		answerable bool
	}{
		{"the script takes answers", true, "", true},
		{"an older script", false, "", false},
		{"a subagent's question", true, "a1", false},
	}
	for _, tc := range cases {
		p, r, _ := newTestProvider(t)
		if tc.canAnswer {
			polledTakingAnswers(p)
		} else {
			p.TakeInbox("s1") // an older script polls without caps
		}
		hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
		hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbQuestion, "agent_id": tc.agent})
		ask := r.events[2]
		require.Equal(t, core.KindAssistantMessage, ask.Kind, tc.name)
		assert.Equal(t, tc.answerable, ask.Ask != nil, tc.name)
	}
}

func TestAsk_UsesTheHooksPromptAfterARestart(t *testing.T) {
	p, r, _ := newTestProvider(t)
	// roomd restarted mid-turn: it never saw the prompt.
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "ExitPlanMode", "tool_use_id": "t1", "tool_input": thePlan, "prompt_id": "p9"})
	assert.Equal(t, "ask:p9:"+printFor("ExitPlanMode", thePlan), r.events[0].ClientEventID)
}

func TestAnswer_ASessionEndingAsAnAnswerArrivesSettlesItOnce(t *testing.T) {
	p, r, c := newTestProvider(t)
	asking(t, p, "AskUserQuestion", dbQuestion)
	wait := waitFor(t, p, "AskUserQuestion", dbQuestion)
	// The answer lands and the session ends before the woken wait runs.
	p.mu.Lock()
	pick := answerCmd(7, "AskUserQuestion", dbQuestion, core.Answer{Questions: []core.QuestionAnswer{{Labels: []string{"Postgres"}}}})
	require.NoError(t, p.deliverAnswer(p.sessions["s1"], pick, c.now()))
	p.forget("s1")
	p.mu.Unlock()

	reply := receive(t, wait)
	assert.True(t, reply.Closed)
	assert.Nil(t, reply.Decision, "never handed over once Room was told it failed")
	assert.ErrorIs(t, r.settled[7], errEnded)
}

func TestAnswer_TheSameQuestionAskedAgainInATurnWaitsAgain(t *testing.T) {
	p, r, _ := newTestProvider(t)
	p.answerHold = 20 * time.Millisecond
	asking(t, p, "AskUserQuestion", dbQuestion)
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbAnswered})
	// Claude asks it again: a new call.
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t2", "tool_input": dbQuestion})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working", "wait:p1:3=needs_input"}, stateChanges(r))
	assert.False(t, waitNow(t, p, "AskUserQuestion", dbQuestion).Closed, "Room can answer it again")
}

func TestAnswer_ALatePreToolUseOfAnAnsweredCallDoesNotAskAgain(t *testing.T) {
	p, r, _ := newTestProvider(t)
	polledTakingAnswers(p)
	hook(t, p, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "go"})
	hook(t, p, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "AskUserQuestion", "tool_input": dbQuestion})
	hook(t, p, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbAnswered})
	hook(t, p, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": dbQuestion})
	assert.Equal(t, []string{"work:p1=working", "wait:p1:1=needs_input", "work:p1:2=working"}, stateChanges(r))
}
