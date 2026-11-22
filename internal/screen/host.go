// Package screen is roomd's side of a screen view: the Host that runs the
// roomd-screen helper, one process per view, and the protocol between them
// (wire.go). roomd never links the capture code itself; see cmd/roomd-screen.
package screen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// Supported is whether this platform can show its screen: macOS only.
const Supported = runtime.GOOS == "darwin"

const (
	// answerWait bounds `roomd-screen session` from spawn to its reply; its
	// own ICE gathering takes at most 2s of it.
	answerWait = 10 * time.Second
	// setupWait bounds `roomd-screen setup`, which only asks macOS and returns.
	setupWait = 30 * time.Second
	// stopGrace is how long a child has to end its view after SIGTERM
	// before it is killed.
	stopGrace = 3 * time.Second
	// maxReply bounds the reply line: an answer SDP is a few KiB.
	maxReply = 256 << 10
	// maxLogLine bounds each roomd-screen log line copied to roomd's log.
	maxLogLine = 500
)

// Host runs roomd-screen: at most one view at a time. It is a core.ScreenHost.
type Host struct {
	path string // the roomd-screen binary

	mu    sync.Mutex
	child *child // the latest view; it may have ended already
}

var _ core.ScreenHost = (*Host)(nil)

// child is one `roomd-screen session`, from before it starts until reaped.
type child struct {
	cancel context.CancelFunc // SIGTERM, then SIGKILL after stopGrace
	done   chan struct{}      // closed once reaped, or if it never started
}

// stop ends the child and waits for it to be reaped (at most stopGrace).
func (c *child) stop() {
	c.cancel()
	<-c.done
}

// NewHost runs the roomd-screen binary at path.
func NewHost(path string) *Host { return &Host{path: path} }

// Beside is roomd-screen's path next to the running roomd, where both
// `make build` and `roomd install` put it.
func Beside() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(self), "roomd-screen"), nil
}

// Answer stops the running view, spawns `roomd-screen session` for offer and
// returns its answer. The child keeps serving the view until ctx ends, Stop
// or the next Answer replaces it, or the view ends by itself.
func (h *Host) Answer(ctx context.Context, offer core.ScreenOffer) (string, error) {
	// The child lives as long as viewCtx: cancelled by Stop, the next Answer,
	// ctx, or by its own exit.
	viewCtx, cancel := context.WithCancel(ctx)
	c := &child{cancel: cancel, done: make(chan struct{})}
	// The new child takes the slot before it starts, so a Stop from now on
	// reaches it, and the old one is gone before it starts: never two at once.
	h.mu.Lock()
	if err := ctx.Err(); err != nil {
		h.mu.Unlock()
		cancel()
		return "", err // superseded already: never displace the newer view
	}
	old := h.child
	h.child = c
	h.mu.Unlock()
	if old != nil {
		old.stop()
	}

	replies := make(chan string, 1)
	cmd := h.command(viewCtx, "session")
	cmd.Stdout = &lineWriter{max: maxReply, line: func(l string) {
		select {
		case replies <- l: // the reply; nothing else is printed
		default:
		}
	}}
	stdin, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start() // fails at once if a Stop already cancelled ctx
	}
	if err != nil {
		cancel()
		close(c.done)
		return "", fmt.Errorf("start roomd-screen: %w", err)
	}
	go func() {
		err := cmd.Wait()
		cancel() // the view is over: release viewCtx
		// A child stopped on purpose exits with ctx's error; only report others.
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("roomd-screen (%s) exited: %v", offer, err)
		}
		close(c.done)
	}()
	go func() {
		// The offer is the only thing sent; stdin then stays open for the
		// whole view, so roomd exiting (EOF) ends it. A child that never reads
		// is killed when its answer times out, which ends this write.
		if err := json.NewEncoder(stdin).Encode(Request{Offer: offer}); err != nil {
			log.Printf("send %s to roomd-screen: %v", offer, err)
		}
	}()

	line, err := awaitReply(ctx, replies, c.done)
	if err != nil {
		c.stop()
		return "", err
	}
	var r Reply
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		c.stop()
		return "", errors.New("roomd-screen sent a reply that is not JSON")
	}
	if r.SDP != "" {
		return r.SDP, nil
	}
	// Having said why, it exits by itself; make sure it does.
	select {
	case <-c.done:
	case <-time.After(stopGrace):
		c.stop()
	}
	switch r.Unavailable {
	case NeedsScreenRecording:
		return "", core.ErrNeedsScreenRecording
	case NeedsAccessibility:
		return "", core.ErrNeedsAccessibility
	}
	return "", errors.New("roomd-screen could not start the view (its log lines say why)")
}

// awaitReply waits up to answerWait for the child's reply line.
func awaitReply(ctx context.Context, replies <-chan string, done <-chan struct{}) (string, error) {
	timer := time.NewTimer(answerWait)
	defer timer.Stop()
	select {
	case line := <-replies:
		return line, nil
	case <-done:
		// A child that replied and exited has its reply waiting: exec copies
		// stdout before Wait returns.
		select {
		case line := <-replies:
			return line, nil
		default:
			return "", errors.New("roomd-screen exited without answering")
		}
	case <-timer.C:
		return "", fmt.Errorf("roomd-screen did not answer within %s", answerWait)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Stop ends the running view, if any, and waits for its process to exit.
func (h *Host) Stop() {
	h.mu.Lock()
	c := h.child
	h.child = nil
	h.mu.Unlock()
	if c != nil {
		c.stop()
	}
}

// Setup asks macOS for the permissions a view needs, opening System Settings
// for any still missing, and reports which are granted. It runs
// `roomd-screen setup` as roomd's child, so macOS attributes the requests to
// roomd rather than to the terminal that asked.
func (h *Host) Setup(ctx context.Context) (core.ScreenPermissions, error) {
	ctx, cancel := context.WithTimeout(ctx, setupWait)
	defer cancel()
	var p core.ScreenPermissions
	out, err := h.command(ctx, "setup").Output()
	if err != nil {
		return p, fmt.Errorf("roomd-screen setup: %w", err)
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return p, errors.New("roomd-screen setup printed something that is not JSON")
	}
	return p, nil
}

// command prepares `roomd-screen <mode>` with its stderr going to roomd's
// log. Ending ctx sends SIGTERM, then SIGKILL after stopGrace.
func (h *Host) command(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, h.path, mode)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace
	cmd.Stderr = &lineWriter{max: maxLogLine + 1, line: func(l string) {
		log.Printf("roomd-screen: %s", core.Truncate(l, maxLogLine))
	}}
	return cmd
}

// lineWriter splits what is written into lines of at most max bytes,
// dropping the excess of a longer line.
type lineWriter struct {
	max  int
	buf  []byte
	line func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		w.buf = append(w.buf, chunk[:min(len(chunk), w.max-len(w.buf))]...)
		if i < 0 {
			break
		}
		w.line(string(w.buf))
		w.buf = w.buf[:0]
		p = p[i+1:]
	}
	return n, nil
}
