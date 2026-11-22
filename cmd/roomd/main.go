// Command roomd is the Room connector: a per-machine daemon that streams the
// agent sessions on this machine to the Room app and delivers the messages
// sent from it.
//
//	roomd install            install the daemon (launchd) and the Claude Code plugin
//	roomd login [--server U] link this machine to your Room account
//	roomd status             show whether roomd runs and is linked
//	roomd logout             unlink this machine
//	roomd screen setup|stop  allow viewing this screen from Room / end a view
//	roomd uninstall          remove the daemon and the plugin
//	roomd run                run in the foreground (what launchd runs)
//	roomd emit <provider>    forward a hook's JSON from stdin (used by the plugin)
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `roomd: the Room connector

  roomd install             install the daemon and the Claude Code plugin
  roomd login [--server U]  link this machine to your Room account
  roomd status              show whether roomd runs and is linked
  roomd logout              unlink this machine
  roomd screen setup        allow viewing this screen from the Room app (macOS)
  roomd screen stop         end the running screen view
  roomd uninstall [--purge] remove the daemon and the plugin
  roomd run                 run in the foreground
  roomd emit <provider>     forward a hook's JSON from stdin (used by plugins)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "run":
		err = runDaemon(ctx)
	case "login":
		err = login(ctx, args)
	case "logout":
		err = logout(ctx)
	case "status":
		err = printStatus(ctx)
	case "screen":
		err = screenCommand(ctx, args)
	case "emit":
		emit(args) // never fails: a hook must not break the session
	case "install":
		err = install(ctx)
	case "uninstall":
		err = uninstall(ctx, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "roomd:", err)
		os.Exit(1)
	}
}
