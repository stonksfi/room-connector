# room-connector

`roomd`, the Room connector: a per-machine daemon that streams the agent
sessions running on this machine to the Room app, and delivers the messages you
send from the app into them. Today it supports Claude Code; another agent is one
more provider (see CLAUDE.md).

```sh
make install        # build, install roomd and roomd-screen (launchd) and the Claude Code plugin
roomd login         # prints a code: enter it in the Room app (Settings > Link a machine)
roomd status        # is it running and linked?
```

`roomd login --server https://<room-be>` points it at a deployed backend; the
default is `http://localhost:8888` (room-be's `make run`). Sessions already open
pick the plugin up after `/reload-plugins`.

On a Mac, the Room app can also view and control this screen. Allow it once, in
person at the Mac:

```sh
roomd screen setup  # asks macOS for Screen Recording and Accessibility for roomd
roomd screen stop   # end a view from the Mac (or use Stop viewing in the menu bar)
```

Go binaries are ad-hoc signed, so macOS forgets those permissions whenever roomd
is reinstalled: run `roomd screen setup` again, or install with
`ROOM_CODESIGN_IDENTITY="<your signing identity>" make install` (see
`security find-identity -p codesigning`) so the grants stick.

Everything lives in `~/.room`: the binaries (`roomd`, `roomd-screen`), config, the offline event queue,
the local socket the plugin talks to, and `roomd.log`. The device token is in
the macOS keychain (`room-connector`). `roomd uninstall --purge` removes it all.
