# @stonksfi/roomd

`roomd`, the Room connector: a small background service on your Mac that shows
your Claude Code sessions in the Room app and delivers the messages you send from
your phone into them.

Needs macOS (Apple Silicon or Intel) and [Claude Code](https://claude.com/claude-code) on your PATH.

```sh
npm install -g @stonksfi/roomd
roomd install      # starts roomd at login and adds the Room plugin to Claude Code
roomd login        # prints a code: enter it in the Room app (Settings > Link a machine)
```

Claude Code sessions that were already open pick up the plugin after `/reload-plugins`.

To view and control this Mac's screen from the app, allow it once at the Mac:

```sh
roomd screen setup   # asks macOS for Screen Recording and Accessibility
```

| | |
|---|---|
| `roomd status` | is it running and linked? |
| `roomd logout` | unlink this machine |
| `roomd screen stop` | end a screen view from the Mac |
| `npm i -g @stonksfi/roomd@latest && roomd install` | upgrade |
| `roomd uninstall --purge && npm rm -g @stonksfi/roomd` | remove everything |

`roomd install` copies roomd to `~/.room/bin`, which is where it runs from, so the
daemon keeps working even if the npm package is removed. Everything else lives in
`~/.room` too, and the device token is in your keychain.

What leaves your Mac: your prompts, Claude's replies, one-line summaries of tool
calls, and each session's folder, name and state. Source:
[stonksfi/room-connector](https://github.com/stonksfi/room-connector).
