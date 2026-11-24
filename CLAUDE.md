# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Workflow

- **Run the code simplifier after making code changes** - After editing source files in a task, run the `/simplify` skill on the changed code, then apply the fixes. Skip for read-only/research/conversational turns and for regenerated code under `internal/model/`.
- **Update tests after making code changes** - Tests live beside the code (`*_test.go`) and run offline with `make test`: no Room server, Claude Code or keychain. Fake the collaborator at its interface (`core.Sink`, `core.Provider`, an `httptest` server for room-be) rather than reaching the network or the OS.
- **After every code change, run the `code-reviewer` agent** on the diff against `.claude/code-review-rules.md`; address its BLOCKER and WARNING findings. To teach it a new class of bug, add a `## Rule:` section to the rules file; never edit the agent.

## Build and Run Commands

```bash
make build            # bin/roomd and bin/roomd-screen (cgo; needs the Xcode command line tools)
make test             # offline test suite
make update-idl       # regenerate internal/model from ../http_idl/room (hz model)
make install          # build, then install the launchd agent and the Claude Code plugin
                      # (signed with this Mac's code signing identity; see Screen view)
make validate-plugin  # `claude plugin validate plugins/claude-code`
make npm-pack V=0.1.0     # release build of both Mac archs into the npm packages, packed into dist/
make npm-publish V=0.1.0  # ...and publish them (`npm login` first)

ROOM_HOME=/tmp/rh ROOM_TOKEN_FILE=/tmp/rh/token ROOM_SOCKET=/tmp/rh.sock ./bin/roomd run
./bin/roomd-screen bench   # capture + encode 5s of the screen; fps, bitrate, encode latency
```

`ROOM_HOME`, `ROOM_SOCKET` and `ROOM_TOKEN_FILE` move roomd's state, socket and
token (a file instead of the keychain) for isolated runs. Unix socket paths are
limited to about 104 bytes, so keep `ROOM_SOCKET` short.

## Architecture Overview

```
agent session ──hooks──▶ roomd emit ──local socket──▶ Provider ──Sink──▶ Uplink ──RPC──▶ room-be
agent session ◀─in-session script polls inbox── Provider ◀─Deliver── Uplink ◀─connector stream── room-be

room-be ──screen_offer──▶ Uplink ──Answer──▶ screen.Host ──stdin──▶ roomd-screen session (one child per view)
room-be ◀─AnswerScreenView{sdp | unavailable}── Uplink ◀──────────── stdout ◀──┘
iPhone ◀═══ H.264 video + "input" data channel (WebRTC, P2P or TURN) ═══▶ roomd-screen: SCK → VideoToolbox → pion; input → CGEventPost
```

- `cmd/roomd/` - the CLI: run, login, logout, status, emit, install, uninstall, screen setup|stop, version.
- `cmd/roomd-screen/` - the screen view helper, a second binary: `session` (what roomd runs), `setup`, `bench`. Its `init` locks the main goroutine to the main thread, which runs the app's event loop.
- `internal/core/` - **the provider-neutral contract**: `Session`, `Event`, `Command`, `Provider`, `Sink`, plus the optional `HookReceiver`, `InboxServer` and `AnswerServer` a provider implements when its agent reports over the local socket; a command's `Kind` (a message, or an `Answer` to an event's `Ask`, with `FitsRoom` and the `MaxAsk*` limits room-be holds an ask to); and the screen view's `ScreenOffer`, `ScreenHost` (`Answer`, `Stop`), `ScreenPermissions` and `ErrNeeds*` (`screen.go`). Nothing in core names an agent.
- `internal/provider/<agent>/` - one package per agent. `claudecode` maps Claude Code's hook JSON to core events, reassembles streamed replies, reconciles against `claude agents --json`, and queues app messages for its in-session script.
- `internal/uplink/` - the room-be client: RPCs, the connector stream, and `Uplink`, the `core.Sink` that batches sessions, events and acks, keeping events on disk until accepted.
- `internal/localapi/` - the HTTP API on `~/.room/roomd.sock`, routed by provider name: `POST /v1/providers/{name}/hook`, `GET /v1/providers/{name}/inbox?session=`, `POST /v1/providers/{name}/inbox/return`, `POST /v1/providers/{name}/answer/wait` (a long poll; `caps=answer` on the inbox poll says the script takes answers), `GET /v1/status`; plus `POST /v1/screen/setup` and `POST /v1/screen/stop` (its own `Screen` interface: `Setup`, `Stop`), and `POST /v1/logout`, which `roomd logout` calls so the daemon drops the deleted token at once.
- `internal/screen/` - roomd's side of a screen view: `Host` spawns, replaces and stops the `roomd-screen session` child (at most one), and `wire.go` is the JSON-lines protocol between them.
- `internal/screen/session/` - roomd-screen only, pure Go: the pion peer that answers the phone, the view state machine, `input` channel parsing, click counting, coordinate mapping and congestion control. `Source`, `Injector` and `Indicator` keep it testable without macOS.
- `internal/screen/macos/` - roomd-screen only, cgo + Objective-C, `darwin && cgo`: ScreenCaptureKit capture encoded by VideoToolbox, `CGEventPost` input, the permissions, the menu bar indicator and its run loop, and the display-awake assertions.
- `internal/store/` - the device token (keychain on macOS, with a timeout), the offline event queue (an append-only journal `queue.jsonl` plus `queue.acked`, dropped by sequence number) and the command log (`commands.json`: which commands were handed to an agent and which outcomes Room is still owed).
- `internal/daemon/` - wires it together; `Providers()` lists the agents this build supports.
- `internal/model/` - generated from the shared IDL (DO NOT EDIT; `make update-idl`).
- `plugins/<agent>/` - the plugin each agent loads, embedded into the binary and written out by `roomd install`.
- `npm/` - the npm packages: `roomd` (`@stonksfi/roomd`, a Node shim that runs the platform binary) and `roomd-darwin-{arm64,x64}` (the binaries, built in by `scripts/npm-release.sh`, which stages them in `dist/npm/`). `roomd install` copies the binary out to `~/.room/bin`, so launchd and the hooks never run Node.

### Releases

- `config.DefaultBaseURL` (localhost) and `main.version` (`dev`) are set at link time by `scripts/npm-release.sh`: production room-be and the release version. Source builds keep the defaults.
- The daemon reads `base_url` from `config.json` once, at start, and `roomd login` defaults `--server` to it. `roomd install` on an unlinked machine saves the installed build's `DefaultBaseURL` there (a release over a dev build moves it to production); once linked, it keeps the server the token was issued by.
- `roomd install` warns about re-running `roomd screen setup` only when the installed `roomd` is unsigned or ad hoc (`codesign -dv`), so a release signed with `ROOM_CODESIGN_IDENTITY` installs quietly.
- `roomd` is built with `CGO_ENABLED=0` for each arch; `roomd-screen` with cgo (go passes clang the matching `-arch`). The platform packages are published before the main one, which pins them to its exact version.

### Architecture Rules

- **One-way dependency flow**: `cmd` → `daemon` → `provider/*`, `uplink`, `localapi`, `screen` → `core`. Providers depend on `core` only; they never import `uplink` or each other. `screen/session` and `screen/macos` are imported only by `cmd/roomd-screen`.
- **roomd links no media framework**: AppKit, ScreenCaptureKit, VideoToolbox and CoreGraphics live only in `roomd-screen`, so `roomd emit`, which runs on every hook, stays small and cgo-free, and a capture crash ends one view, not session streaming. Check with `otool -L bin/roomd`.
- **Providers speak core, the uplink speaks the IDL**: only `uplink` converts to `internal/model` types. A provider never sees room-be's API.
- **Hooks fail open**: `roomd emit` and the plugin never fail, block or slow a session. If roomd is down they do nothing.
- **Every event carries a stable `ClientEventID`** derived from the agent's own ids (prompt id, tool use id, message id), so resending after a crash or an offline stretch is harmless: room-be ignores repeats.
- **Room's full snapshot is device-wide**: the uplink sends one only after every provider has reported, so one agent's snapshot never ends another's sessions.
- **Nothing room-be refuses may stall the uplink**: the uplink truncates every field to room-be's limits (`core.Max*`), caps a push at 1 MB, and drops (with a log line) any item room-be refuses with a permanent 4xx (`uplink.IsPermanent`), bisecting a refused batch to find it. Only network errors, 5xx, 401 and 429 are retried.
- **A command runs at most once**: the command log survives restarts. A command a previous roomd handed to an agent without learning its fate is failed ("send it again"), never re-run. Outcomes are persisted until Room accepts them.
- **No secrets in config or logs**: the device token is kept only in `store`. A screen offer's SDP (it holds IP addresses) and its TURN credentials are never logged or passed as arguments; `core.ScreenOffer` prints as its id only, pion's errors about an SDP are replaced by fixed text, and pion's connection logging (ICE, DTLS, TURN) is silenced.

### Adding an Agent (e.g. Codex)

1. Add `internal/provider/<agent>/` implementing `core.Provider` (and `HookReceiver` / `InboxServer` if its integration calls the local socket). Pick a stable `Name()`: it is stored by room-be as the session's provider.
2. Map the agent's lifecycle onto `core.State` (working, needs input, sleeping, ended) and its conversation onto the five event kinds: the person's message, the agent's reply, a tool call, a state change, and a note (something the agent's harness handed it in the person's turn, with a `Label` the app shows).
3. Implement `Deliver` with whatever in-session mechanism the agent offers, and settle each command through the sink when it lands or fails. To let Room answer the agent's questions, implement `AnswerServer` and set `Event.Ask` only on questions it can answer; otherwise refuse `CommandAnswer` commands.
4. Add it to `daemon.Providers()`, and its plugin, if it has one, under `plugins/` with install steps in `cmd/roomd/install.go`.

room-be and the app need no change: providers are just strings to them.

### Claude Code specifics

- Hooks (`plugins/claude-code/hooks/hooks.json`) call `bin/room-emit`, which runs `~/.room/bin/roomd emit claude_code`. SessionStart, UserPromptSubmit, Stop and SessionEnd are synchronous (order and Stop's answer matter); PreToolUse, PostToolUse, PostToolUseFailure, PermissionRequest, Notification, Elicitation, ElicitationResult, MessageDisplay and StopFailure run in the background (never delaying a prompt, never answered), so MessageDisplay chunks are reassembled by `index` and any of them can arrive late.
- Needs input (`waiting.go`, whose header has the rules): a running turn waits from a PermissionRequest, an Elicitation, a person-facing Notification or an AskUserQuestion/ExitPlanMode PreToolUse, and works again at the asked tool's PostToolUse, an ElicitationResult, or Claude moving on (a tool call or reply it had not started). A denied prompt fires no hook, so moving on is what ends it. Ids: `wait:<prompt>:<n>`, `work:<prompt>:<n>`. A wait's event carries the asked tool's name (`ToolName`, empty for an elicitation or dialog), so the app can say what the person is asked for.
- Questions and plans (`asks.go`): an AskUserQuestion's questions with their options, and an ExitPlanMode's plan, are reported as the agent's message (markdown, `describeAsk`) from its PreToolUse or PermissionRequest, whichever arrives first (`ask:<prompt>:<fingerprint>`, shown once per turn), and get no tool row. It is not the turn's reply: Stop still reports one. The id's prompt is the hook's own `prompt_id` when it has one, so a roomd restarted mid-turn still names it.
- Answering from Room (`answers.go`, whose header has the rules): an event carries a structured `Ask` (`buildAsk`) only when Room can answer it: the script's last poll had `caps=answer` and is fresh, the question is the main loop's (no `agent_id`), every question is a plain choice, and it `FitsRoom`. The script's `classic.PermissionRequest` hook long-polls `answer/wait` beside Claude Code's dialog; roomd fingerprints its tool input like the hooks, so the ask id, the wait and the `ANSWER` command name one question, and answers it with the hook decision. Every state change goes through `enterState`, so leaving a wait always closes it.
- Found by a spike against Claude Code 2.1.330's mod API: a PermissionRequest hook fires as the dialog is drawn and runs beside it, and its answer closes the dialog; its budget is 10s of its own time, a clock that stops during any in-flight `$` call but not a `$.clock` wait, so it waits only on `$.http` (roomd holds each poll up to 25s). A hook's `allow` cannot approve a plan ("for a tool that requires the person ... a hook only tightens"), so plans are approved at the Mac; `deny` + message keeps planning. The ExitPlanMode hook input carries `plan`.
- The in-session script (`hooks/register.ts`, a Claude Code mod) polls the inbox every 1.5s (with `caps=answer`) over the socket and submits each message with `$.prompt.submit({ text, asUser: true })`, which waits for the session to be idle and wakes an idle one. A message counts as delivered when its `UserPromptSubmit` arrives.
- If `prompt.submit` refuses a message (`{ drop }`) or throws, the script returns it through `inbox/return` and it fails with that reason. Polls never overlap and each request times out in 3s.
- When no script polls (mods turned off), the Stop hook (never StopFailure, whose background answer is ignored) answers `{"decision":"block","reason":...}` with the waiting message. It is reported as the person's message then, and settled delivered when the next Stop arrives with `stop_hook_active`, which proves Claude continued.
- Replies: a reply still streaming at Stop is reported with the Stop's full `last_assistant_message` and its id closed, so trailing chunks are ignored; chunks completing after a Stop and before the next prompt are late copies of the reply already reported. A Stop with no reply in progress reports `stop:<prompt>:<n>`.
- A turn that ends with background agents still running is not done: when a Stop's `background_tasks` lists a `subagent` or `workflow` (`reportsBack`), the session stays working (no `sleep:` event), since each one's report starts the next turn; the first Stop that leaves none sleeps. Shells and monitors don't count (a dev server can run for good), matching Claude Code's own busy rule, which `claude agents` reports.
- Untaken or unconfirmed messages only expire (2 min) while the session sleeps or needs input; a running turn can still deliver them.
- Prompts the harness writes fire `UserPromptSubmit` too, the whole prompt wrapped in one tag: a subagent's report (`<agent-message>`), a background task finishing (`<task-notification>`). `readHarness` (`harness.go`, one `harnessTags` entry per tag) reports them as `KindNote` with a label and the body without the tag, a report's preamble or the harness's indent; they still start a turn. A prompt that delivers an app message is matched first and stays the person's; any other tag is the person's too.
- `bin/room-emit` always exits 0 and discards roomd's stderr: exit 2 from a hook blocks the person's prompt.
- `claude agents --json` (every 15s) catches what hooks miss: sessions started before the plugin, Esc-interrupted turns, crashes, and waits no hook saw begin or end (`busy` is working, `waiting` needs input, anything else sleeping). Hook state wins while it is fresh.
- Mod API types: grep the `claude-code.d.ts` the plugin-authoring skill lays down; that API changes between Claude Code releases, so re-run `make validate-plugin` after upgrading Claude Code.

### Screen view

The Room app can view and control this Mac's main display. Video and input go peer to peer over WebRTC (Cloudflare TURN as the fallback relay); room-be only relays one offer and one answer.

- **Flow.** room-be sends `screen_offer` (JSON of `entities.ScreenOffer`) on the connector stream. The uplink answers it on its own goroutine, ignoring it unless the machine is linked: `Host.Answer` spawns `roomd-screen session` beside roomd, writes the offer on stdin and reads one reply line within 10s; the uplink sends it with `AnswerScreenView{offer_id, answer_sdp | unavailable}` (5s deadline; an SDP over `core.MaxAnswerSDP` is sent as `SCREEN_FAILED`). Each view runs on its own context: a permanent refusal of its answer (a 404: nobody waits any more) ends that view only, and a newer offer ends the older view. Any other failure leaves it to the view's 15s connect timeout, since the answer may have got through. room-be holds the phone's request for 20s, so the answer path must stay well under that (about 0.3s spawn to answer when measured).
- **The child** answers non-trickle (gathering capped at 2s): the phone sends a recvonly H.264 offer and creates the `input` data channel. Capture starts only once the peer connects, and input is applied only once capture (and the menu bar indicator) is up; the view ends as `closed` (phone `bye`), `lost` (no connection in 15s, `failed`, or 10s `disconnected`), `stopped_on_mac` (Stop viewing in the menu bar, SIGTERM, or EOF on stdin when roomd goes away) or `capture_failed`, releasing held buttons, then capture, then sending `bye` for the Mac-side reasons, then closing the peer. Encoder bitrate follows GCC between 0.3 and 8 Mbps.
- **The `input` channel, as the phone expects it.** The Mac sends `{t:"display",w,h}` (points) once, as the channel opens, and `bye` with `reason` exactly `stopped_on_mac` or `capture_failed` (anything else reads as lost). A `scroll`'s `dx,dy` is the fingers' movement as fractions of the picture, positive right and down; the content follows the fingers (`macos.wheels`: a positive CG wheel1 moves content down). A phone that leaves early never cancels its offer: the 15s connect timeout is what ends that child.
- **Ending it.** `Host.Stop` (SIGTERM, then SIGKILL after 3s) runs on `roomd screen stop`, `roomd logout` (which also makes the daemon forget its cached token, so no new view starts), any 401 from room-be (a revoked device), and daemon shutdown; the next offer replaces the view. A child takes the Host's slot before it starts, so a Stop never misses one being spawned, and never are two alive.
- **Permissions** are granted in person at the Mac, never by `roomd install`: `roomd screen setup` asks the running daemon to run `roomd-screen setup`, so macOS attributes Screen Recording and Accessibility to roomd (the LaunchAgent), not the terminal. A view that lacks one answers `SCREEN_NEEDS_*`, and the app shows the setup command.
- **Signing.** Go binaries are ad-hoc signed and their signature changes on every build, so macOS forgets the grants after each install. With `ROOM_CODESIGN_IDENTITY` set, `roomd install` signs both binaries with that identity (`com.stonksfi.roomd`, `com.stonksfi.roomd-screen`) and the grants survive rebuilds; otherwise it says to re-run `roomd screen setup`. The Makefile sets it, for `make install` and the npm release, to the keychain's first code signing identity (`security find-identity -p codesigning -v`) unless it is already set; set it empty to sign ad hoc.
- **Known limits:** the main display only; a locked screen can be seen but not typed into, nor can secure input fields; macOS Sequoia asks again monthly whether roomd may keep capturing (the phone reports no picture after 8s meanwhile); one view at a time.
- `roomd-screen bench` is the capture check: 5s of the screen to a playable `.h264`, with fps, bitrate and encode latency. Frames flow only while the screen changes.
