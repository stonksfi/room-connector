import type { Register } from 'claude-code'

// Delivers messages from the Room app into this session. roomd queues them per
// session; this polls its inbox over roomd's local socket and submits each one
// as the person's own prompt. A plugin's prompt waits until the session is idle,
// so a message sent mid-turn runs right after the turn, and one sent to an
// idle session wakes it.
//
// Nothing here may disturb the session: every call is caught, a stalled roomd
// costs one timed-out request, and polls never overlap.

const PROVIDER = 'claude_code'
const POLL_MS = 1500
const FETCH_TIMEOUT_MS = 3000

type Inbox = { messages?: { command_id: number; text: string }[] }

export const register: Register = (on) => {
  on('session.start', async ($, e, next) => {
    const result = await next(e)
    if (!e.isInteractive) {
      return result // a -p run has no prompt to submit into
    }

    const socketPath = async (): Promise<string> => {
      const explicit = await $.env.get('ROOM_SOCKET')
      if (explicit) {
        return explicit
      }
      const home = (await $.env.get('ROOM_HOME')) ?? `${(await $.env.get('HOME')) ?? ''}/.room`
      return `${home}/roomd.sock`
    }

    const call = async (path: string, init: { method?: string; body?: string } = {}) => {
      const socket = await socketPath()
      const timeout = $.clock.sleep(FETCH_TIMEOUT_MS).then(() => undefined)
      return Promise.race([$.http.fetch(`http://roomd${path}`, { ...init, socketPath: socket }), timeout])
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
        const res = await call(`/v1/providers/${PROVIDER}/inbox?session=${encodeURIComponent(id)}`)
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
}
