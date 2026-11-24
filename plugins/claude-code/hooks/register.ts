import type { EngineInterface, PermissionRequestDecision, Register } from 'claude-code'

// Delivers messages from the Room app into this session. roomd queues them per
// session; this polls its inbox over roomd's local socket and submits each one
// as the person's own prompt. A plugin's prompt waits until the session is idle,
// so a message sent mid-turn runs right after the turn, and one sent to an
// idle session wakes it.
//
// It also answers a question (AskUserQuestion) or keeps a plan going
// (ExitPlanMode, with the person's feedback) from Room while Claude Code's own
// dialog is open: whichever answer comes first, the Mac's or Room's, wins.
//
// Nothing here may disturb the session: every call is caught, a stalled roomd
// costs one timed-out request, and polls never overlap.

const PROVIDER = 'claude_code'
const POLL_MS = 1500
const FETCH_TIMEOUT_MS = 3000
// How long a question waits for an answer from Room before leaving it to the
// dialog alone, and the most one wait request may take (roomd holds it 25s).
const ANSWER_WAIT_MS = 30 * 60 * 1000
const WAIT_REQUEST_MS = 30 * 1000

type Inbox = { messages?: { command_id: number; text: string }[] }
type WaitReply = { decision?: PermissionRequestDecision; closed?: boolean }

async function socketPath($: EngineInterface): Promise<string> {
  const explicit = await $.env.get('ROOM_SOCKET')
  if (explicit) {
    return explicit
  }
  const home = (await $.env.get('ROOM_HOME')) ?? `${(await $.env.get('HOME')) ?? ''}/.room`
  return `${home}/roomd.sock`
}

function roomd($: EngineInterface, socket: string, path: string, init: { method?: string; body?: string } = {}) {
  return $.http.fetch(`http://roomd${path}`, { ...init, socketPath: socket })
}

export const register: Register = (on) => {
  on('session.start', async ($, e, next) => {
    const result = await next(e)
    if (!e.isInteractive) {
      return result // a -p run has no prompt to submit into
    }

    const socket = socketPath($) // once: the environment does not change
    const call = async (path: string, init: { method?: string; body?: string } = {}) => {
      const timeout = $.clock.sleep(FETCH_TIMEOUT_MS).then(() => undefined)
      return Promise.race([roomd($, await socket, path, init), timeout])
    }

    const giveBack = async (session: string, commandId: number, reason: string) => {
      try {
        await call(`/v1/providers/${PROVIDER}/inbox/return`, {
          method: 'POST',
          body: JSON.stringify({ session, command_id: commandId, reason }),
        })
      } catch {
        // roomd will time the message out instead
      }
    }

    let polling = false
    $.clock.every(POLL_MS, async () => {
      if (polling) {
        return // the last poll is still submitting
      }
      polling = true
      try {
        const id = await $.session.id() // read each time: /clear starts a new session
        // caps=answer: this script answers questions from Room (below).
        const res = await call(`/v1/providers/${PROVIDER}/inbox?session=${encodeURIComponent(id)}&caps=answer`)
        if (!res?.ok) {
          return
        }
        const inbox = JSON.parse(res.text) as Inbox
        const messages = inbox.messages ?? []
        for (let i = 0; i < messages.length; i++) {
          const m = messages[i]!
          let reason: string | undefined
          try {
            const submitted = await $.prompt.submit({ text: m.text, asUser: true })
            if ('drop' in submitted) {
              reason = String(submitted.drop)
            }
          } catch (err) {
            reason = String(err)
          }
          if (reason !== undefined) {
            // Hand this one and the rest back rather than leave them hanging.
            for (const rest of messages.slice(i)) {
              await giveBack(id, rest.command_id, reason)
            }
            return
          }
        }
      } catch {
        // roomd is not running, or answered something unreadable: next tick
      } finally {
        polling = false
      }
    })
    return result
  })

  // Claude Code asks this as a question or plan dialog opens, and runs it
  // beside the dialog. It waits on roomd, one long poll at a time (roomd holds
  // each up to 25s), until Room has the person's answer or the dialog closes.
  // The hook has 10s of its own time, but its clock stops while a $.http call
  // is in flight and not during a $.clock wait, so it never waits on the
  // clock. A plan can be kept going from Room but not approved: Claude Code
  // leaves that to the person at the Mac.
  on('classic.PermissionRequest', async ($, e, next) => {
    const below = await next(e)
    if (below.decision || e.agent_id || (e.tool_name !== 'AskUserQuestion' && e.tool_name !== 'ExitPlanMode')) {
      return below // decided beneath, a subagent's, or not a question
    }
    const body = JSON.stringify({ session: e.session_id, tool_name: e.tool_name, tool_input: e.tool_input })
    const until = Date.now() + ANSWER_WAIT_MS
    try {
      const socket = await socketPath($)
      while (!next.signal.aborted && Date.now() < until) {
        // Ends early when the dialog's dispatch does (the person answered at
        // the Mac) or roomd stalls; a $.http call in flight beside it keeps
        // the wait off the hook's budget.
        const gaveUp = $.clock.sleep(WAIT_REQUEST_MS, { signal: next.signal }).then(
          () => undefined,
          () => undefined,
        )
        const res = await Promise.race([
          roomd($, socket, `/v1/providers/${PROVIDER}/answer/wait`, { method: 'POST', body }),
          gaveUp,
        ])
        if (!res?.ok) {
          return below // gave up, an older roomd, or one that refused the wait
        }
        const reply = JSON.parse(res.text) as WaitReply
        if (reply.closed) {
          return below // answered at the Mac, or the turn moved on
        }
        if (reply.decision) {
          $.ui.toast('Answered from Room')
          return { ...below, decision: reply.decision }
        }
      }
    } catch {
      // roomd is not running: the dialog stays the person's
    }
    return below
  })
}
