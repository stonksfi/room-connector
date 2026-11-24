package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// Answers from Room. The in-session script's PermissionRequest hook runs
// beside Claude Code's own question or plan dialog and long-polls WaitAnswer
// with the hook's tool name and input; roomd fingerprints them as the hooks
// do, so the wait, the ask event (ask:<prompt>:<print>) and the answer all
// name the question by the same print. First answer wins: the person's at the
// Mac or the one from Room. Claude Code lets a hook answer a question
// (allow with the answers) or keep planning (deny with feedback), but never
// approve a plan: that stays the person's at the Mac.
//
// An answer is settled by what the hooks see, not by the script: a question's
// answer is delivered when its PostToolUse carries the same answers (other
// answers: it was answered at the Mac); plan feedback is delivered when the
// wait ends without the plan's PostToolUse (Claude kept planning). An answer
// still queued when its question closes was too late. Known limit: hooks run
// in the background, so a plan approved at the Mac the moment Room's feedback
// lands can be seen moving on before its PostToolUse, and the feedback reads
// as delivered.

const (
	// answerHold is how long WaitAnswer holds a request with nothing to say.
	answerHold = 25 * time.Second
	// waitFresh is how recently a wait must have run for its question to
	// count as answerable from Room.
	waitFresh = 3 * time.Second
	// untakenAnswerTimeout fails an answer no wait picked up.
	untakenAnswerTimeout = 10 * time.Second
	// printLen is how much of a fingerprint names a question.
	printLen = 16
)

var (
	errNotOpen       = errors.New("the question is no longer open: answer it at the Mac")
	errNoWait        = errors.New("this session can't take answers from Room (its Room plugin is off or out of date): answer it at the Mac")
	errAnswered      = errors.New("the question already has an answer from Room")
	errAnsweredOnMac = errors.New("the question was answered at the Mac")
	errApprovedOnMac = errors.New("the plan was approved at the Mac")
	errAnswerUnused  = errors.New("the session took the answer but never used it")
	errAnswerShape   = errors.New("the answer doesn't fit the question")
)

// pendingAnswer is an answer from Room to one of the session's questions.
type pendingAnswer struct {
	cmd core.Command
	// print names the question it answers.
	print string
	at    time.Time
	// taken: handed to the script, which answered the dialog with it.
	taken bool
}

// waitState is the script's waits on one question: how many are open, and
// when one last ended.
type waitState struct {
	open int
	seen time.Time
}

// waitRequest is what the in-session script's hook sends to WaitAnswer.
type waitRequest struct {
	Session   string         `json:"session"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

// waitReply is WaitAnswer's answer: the hook's decision to return, closed
// when the question closed (the script stops waiting), or neither (poll again).
type waitReply struct {
	Decision map[string]any `json:"decision,omitempty"`
	Closed   bool           `json:"closed,omitempty"`
}

// printOf is the part of a fingerprint that names a question.
func printOf(fp string) string {
	if len(fp) < printLen {
		return fp
	}
	return fp[:printLen]
}

// printFor names the question a tool call asks.
func printFor(tool string, input map[string]any) string {
	return printOf(fingerprint(hookInput{ToolName: tool, ToolInput: input}))
}

// askPrint reads the print out of an ask id (ask:<prompt>:<print>).
func askPrint(askID string) string {
	if !strings.HasPrefix(askID, "ask:") {
		return ""
	}
	return askID[strings.LastIndex(askID, ":")+1:]
}

// CanAnswer records that the session's script takes answers from Room.
func (p *Provider) CanAnswer(providerSessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.get(providerSessionID).takesAnswers = true
}

// deliverAnswer queues an answer for the wait on its question, or refuses it
// when that question is not open with a wait running; the caller holds p.mu.
func (p *Provider) deliverAnswer(s *session, cmd core.Command, now time.Time) error {
	if cmd.Answer == nil {
		return errAnswerShape
	}
	print := askPrint(cmd.Answer.AskID)
	switch {
	case print == "" || s.info.State != core.StateNeedsInput || printOf(s.waitTool) != print || slices.Contains(s.closedAsks, print):
		return errNotOpen
	case !s.waitLive(print, now):
		return errNoWait
	case slices.ContainsFunc(s.answers, func(a pendingAnswer) bool { return a.print == print }):
		return errAnswered
	}
	s.answers = append(s.answers, pendingAnswer{cmd: cmd, print: print, at: now})
	s.wakeWaits()
	return nil
}

// waitLive reports whether a wait for the question is open or just ended.
func (s *session) waitLive(print string, now time.Time) bool {
	w := s.waits[print]
	return w != nil && (w.open > 0 || now.Sub(w.seen) <= waitFresh)
}

// wakeWaits wakes every WaitAnswer of the session to look again.
func (s *session) wakeWaits() {
	if s.wake != nil {
		close(s.wake)
		s.wake = nil
	}
}

// WaitAnswer holds the script's request until its question has an answer
// from Room, closes, or answerHold passes.
func (p *Provider) WaitAnswer(ctx context.Context, body []byte) ([]byte, error) {
	var req waitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode wait: %w", err)
	}
	if req.Session == "" || !asksPerson[req.ToolName] {
		return nil, errors.New("a session and a question tool are required")
	}
	print := printFor(req.ToolName, req.ToolInput)
	ctx, cancel := context.WithTimeout(ctx, p.answerHold)
	defer cancel()

	p.mu.Lock()
	s := p.get(req.Session)
	w := s.waits[print]
	if w == nil {
		w = &waitState{}
		s.waits[print] = w
	}
	w.open++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		w.open--
		w.seen = p.now()
		p.mu.Unlock()
	}()

	for {
		reply, wake := p.lookAtWait(s, req, print)
		if wake == nil {
			return json.Marshal(reply)
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return json.Marshal(waitReply{}) // nothing yet: the script asks again
		}
	}
}

// lookAtWait answers a wait when it can (an answer to hand over, or the
// question closed), else returns what wakes it to look again.
func (p *Provider) lookAtWait(s *session, req waitRequest, print string) (waitReply, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions[req.Session] != s || slices.Contains(s.closedAsks, print) {
		return waitReply{Closed: true}, nil // the session ended, or the question closed
	}
	for i := range s.answers {
		a := &s.answers[i]
		if a.print != print || a.taken {
			continue
		}
		decision, err := decisionFor(req.ToolName, req.ToolInput, a.cmd.Answer)
		if err != nil {
			p.sink.Settle(a.cmd.ID, err)
			s.answers = slices.Delete(s.answers, i, i+1)
			break
		}
		a.taken, a.at = true, p.now()
		return waitReply{Decision: decision}, nil
	}
	if s.wake == nil {
		s.wake = make(chan struct{})
	}
	return waitReply{}, s.wake
}

// decisionFor is the PermissionRequest decision that gives Claude Code the
// person's answer: a question allowed with its answers, a plan refused with
// the feedback (Claude keeps planning).
func decisionFor(tool string, input map[string]any, a *core.Answer) (map[string]any, error) {
	if tool == "ExitPlanMode" {
		feedback := strings.TrimSpace(a.Feedback)
		if feedback == "" {
			return nil, errAnswerShape
		}
		return map[string]any{
			"behavior": "deny",
			"message":  "The person kept planning from the Room app on their phone, with this feedback:\n\n" + feedback,
		}, nil
	}
	answers, err := answersFor(input, a)
	if err != nil {
		return nil, err
	}
	updated := maps.Clone(input)
	updated["answers"] = answers
	return map[string]any{"behavior": "allow", "updatedInput": updated}, nil
}

// answersFor is an AskUserQuestion's answers as Claude Code reads them: by
// question, the labels picked joined with ", ", or the text typed instead.
func answersFor(input map[string]any, a *core.Answer) (map[string]any, error) {
	questions, _ := readQuestions(input)
	if len(questions) == 0 || len(questions) != len(a.Questions) {
		return nil, errAnswerShape
	}
	answers := make(map[string]any, len(questions))
	for i, q := range questions {
		answer := strings.Join(a.Questions[i].Labels, ", ")
		if answer == "" {
			answer = strings.TrimSpace(a.Questions[i].Text)
		}
		if q.Question == "" || answer == "" {
			return nil, errAnswerShape
		}
		answers[q.Question] = answer
	}
	return answers, nil
}

// closeWait ends the wait the session leaves: an answer still queued for it
// was too late, taken plan feedback is delivered (Claude went on without the
// plan), and a taken question answer waits for its PostToolUse. The caller
// holds p.mu.
func (p *Provider) closeWait(s *session, now time.Time) {
	p.closeAsk(s, printOf(s.waitTool), func(a pendingAnswer) bool {
		switch {
		case !a.taken:
			p.sink.Settle(a.cmd.ID, errAnsweredOnMac)
		case a.cmd.Answer.Feedback != "":
			p.answered(s, a, now)
		default:
			return false
		}
		return true
	})
}

// answerUsed settles the answers to the question tool that just ran, by its
// PostToolUse: the same answers mean Room's landed. It runs before the wait
// closes, so plan feedback is not taken as delivered.
func (p *Provider) answerUsed(s *session, in hookInput, now time.Time) {
	var print string
	if i := s.startedTool(in.ToolUseID); i >= 0 {
		print = printOf(s.tools[i].fingerprint)
	} else {
		// Its PreToolUse is late: remember the call, so that hook is not
		// taken for the question being asked again.
		fp := questionPrint(in)
		s.tools = remember(s.tools, toolCall{id: in.ToolUseID, fingerprint: fp})
		print = printOf(fp)
	}
	p.closeAsk(s, print, func(a pendingAnswer) bool {
		switch {
		case in.ToolName == "ExitPlanMode":
			p.sink.Settle(a.cmd.ID, errApprovedOnMac)
		case a.taken && sameAnswers(in.ToolInput, a.cmd.Answer):
			p.answered(s, a, now)
		default:
			p.sink.Settle(a.cmd.ID, errAnsweredOnMac)
		}
		return true
	})
}

// closeAsk records the question closed, wakes its waits, and hands each of
// its answers to settle, which reports whether it settled one; any other (a
// taken question answer before its PostToolUse) stays queued.
func (p *Provider) closeAsk(s *session, print string, settle func(pendingAnswer) bool) {
	if print == "" {
		return
	}
	if !slices.Contains(s.closedAsks, print) {
		s.closedAsks = remember(s.closedAsks, print)
	}
	s.answers = slices.DeleteFunc(s.answers, func(a pendingAnswer) bool {
		return a.print == print && settle(a)
	})
	s.wakeWaits()
}

// questionPrint fingerprints a question tool's call as it was asked: its
// PostToolUse input carries the answers too.
func questionPrint(in hookInput) string {
	if in.ToolName == "AskUserQuestion" {
		asked := maps.Clone(in.ToolInput)
		delete(asked, "answers")
		delete(asked, "annotations")
		in.ToolInput = asked
	}
	return fingerprint(in)
}

// sameAnswers reports whether an AskUserQuestion's input carries the answers
// Room sent.
func sameAnswers(input map[string]any, a *core.Answer) bool {
	want, err := answersFor(input, a)
	got, _ := input["answers"].(map[string]any)
	if err != nil || len(got) != len(want) {
		return false
	}
	for q, w := range want {
		if g, ok := got[q].(string); !ok || g != w {
			return false
		}
	}
	return true
}

// answered settles an answer delivered and shows it as the person's message.
func (p *Provider) answered(s *session, a pendingAnswer, now time.Time) {
	p.sink.Settle(a.cmd.ID, nil)
	p.emit(s, core.Event{
		ClientEventID: "answer:" + fmt.Sprint(a.cmd.ID),
		Kind:          core.KindUserMessage,
		Text:          a.cmd.Text,
		CommandID:     a.cmd.ID,
	}, now)
}

// expireAnswers fails answers no wait picked up, and taken ones never seen
// used while the session is not running a turn; the caller holds p.mu.
func (p *Provider) expireAnswers(s *session, now time.Time) {
	s.answers = slices.DeleteFunc(s.answers, func(a pendingAnswer) bool {
		switch {
		case !a.taken && now.Sub(a.at) > untakenAnswerTimeout && !s.waitLive(a.print, now):
			p.sink.Settle(a.cmd.ID, errNoWait)
		case a.taken && s.info.State != core.StateWorking && now.Sub(a.at) > unconfirmedTimeout:
			p.sink.Settle(a.cmd.ID, errAnswerUnused)
		default:
			return false
		}
		return true
	})
}
