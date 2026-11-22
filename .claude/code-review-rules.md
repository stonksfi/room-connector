# Code Review Rules (room-connector)

The single source of truth for the `code-reviewer` agent. Each `## Rule:` is one class of
bug that matters in roomd: a daemon that sits inside every agent session on the machine.
Rules carried over from room-be/tracker-be keep their intent and are restated for a daemon.

## Rule: Hooks and in-session scripts fail open

Anything an agent session runs (`roomd emit`, `plugins/*/bin/*`, the mod in
`hooks/register.ts`) must never fail, block or noticeably slow the session, whether roomd
is running or not. A non-zero exit, a hang or output the agent misreads breaks the person's
work, which is far worse than a missed Room event.

How to check: for every change under `cmd/roomd/run.go` (`emit`), `plugins/` and
`internal/localapi`, confirm: errors are swallowed to a silent exit 0; every network or
socket call has a timeout of a few seconds; stdin is drained even when roomd is absent;
nothing is printed to stdout unless it is the provider's deliberate reply (e.g. a Stop
decision); the mod catches every rejected `$` call.

Bad:
```go
resp, err := client.Post(url, "application/json", body)
if err != nil { log.Fatal(err) } // exits 1 inside the agent's hook
```
Good:
```go
resp, err := client.Post(url, "application/json", body)
if err != nil { return } // hooks fail open
```

## Rule: Every event has a stable client event id

room-be drops repeats by `client_event_id`, which is what makes resending after a crash,
an offline stretch or a retried batch harmless. An id built from time, a counter or a random
value turns every retry into a duplicate row in the person's transcript.

How to check: for each `core.Event` a provider emits, the `ClientEventID` must be derived from
the agent's own identifiers (prompt id, tool use id, message id, command id). A timestamp is
acceptable only for an event that genuinely happens once per moment (a state change with no
id of its own) and must be combined with the session's identity.

Bad: `ClientEventID: uuid.NewString()`
Good: `ClientEventID: "tool:" + in.ToolUseID`

## Rule: Never swallow an error silently in the daemon

Outside the fail-open boundary above, an error is logged with context or returned. A silent
`_ =` hides why Room stopped updating. Deliberate ignores (best-effort cleanup, an "already
exists" from an idempotent command) carry a comment saying why.

How to check: grep the diff for `_ =`, `_, _ =`, empty `if err != nil {}` and ignored return
values of functions returning `error`. Each needs a log line, a return, or a justifying comment.

Bad: `_ = u.queue.Push(e)`
Good:
```go
if err := u.queue.Push(e); err != nil {
	log.Printf("queue event: %v", err)
}
```

## Rule: Propagate the context; long-lived loops end with it

Every blocking call (HTTP, exec, sleeps, tickers) takes the caller's `ctx`, and every
goroutine or loop started by the daemon returns when that `ctx` ends. A loop that ignores
`ctx` keeps roomd from shutting down when launchd stops it.

How to check: new `http.NewRequest` must be `NewRequestWithContext`; `exec.Command` in a
loop must be `exec.CommandContext`; `time.Sleep` in a loop must be a `select` on `ctx.Done()`;
every `for {}` in a goroutine has a `ctx.Done()` exit. `context.Background()` is only for
short, commented best-effort work (e.g. shutdown with its own timeout).

## Rule: Do not call out of a lock into code that may call back

Providers hold their mutex while reporting to the `Sink`; the uplink calls providers'
`Deliver` from its stream goroutine. If either side calls the other while holding a lock the
other may need, roomd deadlocks and every session's hooks start timing out.

How to check: for any method that holds a mutex, list every call made before unlocking.
A call into another component is allowed only if that component never calls back into
this one synchronously (today: `Sink` methods only lock the uplink and push to the queue;
`Uplink.dispatch` calls `Deliver` with no uplink lock held). A new callback path, or a
blocking network call under a lock, is a BLOCKER.

## Rule: Bound everything that grows

roomd runs for weeks. Any map, slice, queue or buffer fed by events must have a bound or an
eviction path: per-session state is dropped when the session ends or vanishes; the offline
queue drops its oldest past `MaxQueued`; streamed reply drafts are flushed at Stop.

How to check: for each new collection that grows per event, session or command, find the
code that removes entries. Missing removal, or removal only on a path that may never run,
is a WARNING.

## Rule: Command delivery is settled exactly once

Every command handed to a provider must eventually be settled through the sink, delivered
or failed with a reason the person can act on, and never both or twice. An unsettled command
shows as pending in the app forever; a double settle reports a wrong final status.

How to check: trace each new path that takes a command out of a provider's inbox or handed
list. It must either call `Settle` and remove the command, or leave it in a list the expiry
sweep covers. Removing a command without settling it, or settling without removing it, is a
BLOCKER.

## Rule: Providers stay provider-neutral at the boundary

`internal/core`, `internal/uplink`, `internal/localapi` and room-be's API must not learn
about any one agent. Agent-specific knowledge (hook names, CLI flags, file formats) lives in
`internal/provider/<agent>` and `plugins/<agent>`, so adding an agent touches nothing else.

How to check: grep the diff outside `internal/provider/` and `plugins/` for agent names
("claude", "codex"), hook event names or agent file paths. Each hit is a WARNING unless it is
the single provider list in `daemon.Providers()` or the installer for that agent's plugin.

## Rule: No secrets in arguments, logs or config

The device token authenticates the machine to the person's Room account. It is stored only
by `internal/store` (keychain or a 0600 file), never written to `config.json`, never logged,
and never passed on a command line other processes can read. The same holds for a screen
view's SDP (offer and answer: they carry the machine's and the phone's IP addresses) and its
TURN credentials (`core.ICEServer.Credential`): they travel only in RPC bodies and on
roomd-screen's stdin/stdout, never in a log line, an error string or an argument.
`core.ScreenOffer` and `core.ICEServer` print redacted, so log the offer value itself, never
its fields.

How to check: grep the diff for the token's flow, and for every use of `.SDP`, `AnswerSdp`,
`LocalDescription()`, `Credential` and `Username`. Any `log.*`, `fmt.Print*`, error string,
`exec.Command` argument or config field that can contain one is a BLOCKER. roomd-screen's
stderr is copied into roomd's log, so anything it writes there counts as logged.

Bad: `log.Printf("answer for %s: %s", offer.ID, sdp)`
Good: `log.Printf("%s: answered", offer)` (prints `screen offer so_…`)

## Rule: Nothing room-be refuses may stall the uplink

The uplink sends in order and retries what fails. An item room-be will refuse every time (an
oversized text, an unknown enum, a 403 on a command another device owns) must be dropped with
a log line, never retried forever, or one bad row blocks every session's updates and every
command ack behind it, across restarts, because the queue is on disk.

How to check: for every new field sent to room-be, confirm the uplink holds it to room-be's
limit (`core.Truncate` with a `core.Max*` constant, or a validated enum). For every new RPC
call in a retry loop, confirm a permanent refusal (`uplink.IsPermanent`) ends the retry for
that item and lets the rest through. A retry of a 4xx other than 401/408/429 is a BLOCKER.

Motivating bug: a 250 KB pasted prompt exceeded room-be's text limit; every push of the oldest
batch got a 400 and the queue stopped draining for good.

## Rule: Only synchronous hooks can answer

Claude Code ignores the stdout of a hook marked `"async": true` in `hooks.json`. A provider
that answers such a hook (a Stop decision, injected context) loses the answer, and any state
it changed on the assumption that the answer landed is now wrong.

How to check: for each hook event a provider answers with a non-empty reply, confirm the
event is synchronous in `plugins/<agent>/hooks/hooks.json`, and that the handler for every
asynchronous event returns no reply and does not consume inbox messages or settle commands.

Motivating bug: StopFailure (async) went through the Stop path, took a phone message from the
inbox, settled it delivered and answered a block decision nobody read: the message was lost.

## Rule: Screen capture and input live only in roomd-screen, only while a viewer is connected

Seeing and controlling the screen is the most sensitive thing roomd can do. The capture,
encoder, input injection and permission code (AppKit, ScreenCaptureKit, VideoToolbox,
CoreGraphics, cgo) live only in `internal/screen/macos` and `internal/screen/session`, which
only `cmd/roomd-screen` imports: `roomd emit` runs on every hook and must not load them, and a
crash there must end one view, not session streaming. And a view captures and injects only
while its phone is connected: capture starts on the peer's `connected`, everything is
released when the view ends, and every way of ending it (phone `bye`, lost connection, Stop
viewing, `roomd screen stop`, logout, a revoked device, a newer offer, roomd exiting) reaches
the child.

How to check: `go list -deps ./cmd/roomd` must not list `screen/session`, `screen/macos` or
`pion`; a new `import "C"` or framework outside `internal/screen/macos` is a BLOCKER. Code
that starts capture, posts input or shows the indicator must sit behind the connected state
in `session.View`; a path that skips releasing held buttons or stopping capture on end is a
BLOCKER. A new way to end access (a new revoke path, a new logout) that does not stop the
child, or a second child alive at once, is a WARNING. `roomd install` must never request the
permissions itself.

Bad: `src.Start(...)` in `session.Answer`, before the phone has connected.
Good: capture starts in `View.serve` on `PeerConnectionStateConnected`, and `View.end` stops it.
