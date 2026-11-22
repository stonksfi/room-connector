package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stonksfi/room-connector/internal/config"
	"github.com/stonksfi/room-connector/internal/core"
	"github.com/stonksfi/room-connector/internal/localapi"
)

// screenCommand is `roomd screen setup|stop`. Both go through the running daemon: a
// view is its child, and macOS attributes a permission request to the
// process that makes it, which must be roomd, not this terminal.
func screenCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: roomd screen setup|stop")
	}
	switch args[0] {
	case "setup":
		return screenSetup(ctx)
	case "stop":
		return screenStop(ctx)
	}
	return errors.New("usage: roomd screen setup|stop")
}

func screenSetup(ctx context.Context) error {
	var p core.ScreenPermissions
	if err := daemonPost(ctx, "/v1/screen/setup", time.Minute, &p); err != nil {
		return err
	}
	fmt.Print(describePermissions(p))
	return nil
}

func screenStop(ctx context.Context) error {
	if err := daemonPost(ctx, "/v1/screen/stop", 10*time.Second, nil); err != nil {
		return err
	}
	fmt.Println("No screen view is running now.")
	return nil
}

// tellDaemonLoggedOut has a running roomd drop the deleted token at once and
// end any screen view, rather than use its cached copy for another minute.
func tellDaemonLoggedOut(ctx context.Context) {
	err := daemonPost(ctx, "/v1/logout", 5*time.Second, nil)
	if err != nil && !errors.Is(err, errNotRunning) { // one not running holds nothing to drop
		fmt.Printf("! roomd did not take the logout (%v): restart it with `roomd install`, or run `roomd screen stop`\n", err)
	}
}

var errNotRunning = errors.New("roomd is not running: start it with `roomd install` (or `roomd run`)")

// daemonPost calls the running roomd's local API, decoding its JSON answer
// into out unless out is nil.
func daemonPost(ctx context.Context, path string, timeout time.Duration, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://roomd"+path, nil)
	if err != nil {
		return err
	}
	resp, err := localapi.Client(config.SocketPath(), timeout).Do(req)
	if err != nil {
		return errNotRunning
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 4<<10)
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(body) // the status alone is enough if this fails
		return fmt.Errorf("roomd: %s", strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("read roomd's answer: %w", err)
	}
	return nil
}

// describePermissions says what a screen view still needs, and where to
// grant it.
func describePermissions(p core.ScreenPermissions) string {
	var missing []string
	if !p.ScreenRecording {
		missing = append(missing, "Privacy & Security > Screen & System Audio Recording")
	}
	if !p.Accessibility {
		missing = append(missing, "Privacy & Security > Accessibility")
	}
	if len(missing) == 0 {
		return "Ready: roomd may record and control this screen, so you can view it from the Room app.\n"
	}
	return "To view this screen from the Room app, turn on roomd (and roomd-screen, if it is listed) in System Settings:\n  " +
		strings.Join(missing, "\n  ") +
		"\nSystem Settings is open at the first one. Run `roomd screen setup` again once they are on.\n"
}
