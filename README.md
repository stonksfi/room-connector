# room-connector

`roomd`, the Room connector: a per-machine daemon that streams the agent
sessions running on this machine to the Room app, and delivers the messages you
send from the app into them. Today it supports Claude Code; another agent is one
more provider (see CLAUDE.md).

## Install

On a Mac with Claude Code on PATH:

```sh
npm install -g @stonksfi/roomd
roomd install       # roomd and roomd-screen (launchd) and the Claude Code plugin
roomd login         # prints a code: enter it in the Room app (Settings > Link a machine)
roomd status        # is it running and linked?
```

Sessions already open pick the plugin up after `/reload-plugins`. Upgrade with
`npm i -g @stonksfi/roomd@latest && roomd install`; remove everything with
`roomd uninstall --purge && npm rm -g @stonksfi/roomd`.

## From source

```sh
make install                     # build, then `roomd install` (Go, cgo, Xcode command line tools)
~/.room/bin/roomd login          # this build talks to http://localhost:8888 (room-be's `make run`)
```

`roomd login --server https://<room-be>` points a source build at a deployed
backend; npm releases already default to production.

On a Mac, the Room app can also view and control this screen. Allow it once, in
person at the Mac:

```sh
roomd screen setup  # asks macOS for Screen Recording and Accessibility for roomd
roomd screen stop   # end a view from the Mac (or use Stop viewing in the menu bar)
```

macOS keeps those permissions across reinstalls only for a signed roomd.
`make install` signs with the first code signing identity in your keychain
(`security find-identity -p codesigning -v`), or the one
`ROOM_CODESIGN_IDENTITY` names. With none (or `ROOM_CODESIGN_IDENTITY=`) the
binaries are ad-hoc signed, and you run `roomd screen setup` again after each
install.

Everything lives in `~/.room`: the binaries (`roomd`, `roomd-screen`), config, the offline event queue,
the local socket the plugin talks to, and `roomd.log`. The device token is in
the macOS keychain (`room-connector`). `roomd uninstall --purge` removes it all.

## Releasing to npm

`make npm-pack V=0.1.0` builds both Mac architectures into the packages under
`npm/` (staged in `dist/npm/`, pointed at the production room-be) and packs them
into `dist/`; `make npm-publish V=0.1.0` publishes them (`npm login` first, as a
member of the `stonksfi` npm org). The main package, `@stonksfi/roomd`, is a
small Node shim that runs the binary from `@stonksfi/roomd-darwin-arm64` or
`-x64`, whichever npm installed. Releases are signed like `make install`, so
installs keep their screen permissions across upgrades.
