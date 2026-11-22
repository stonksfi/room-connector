package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/stonksfi/room-connector/internal/core"
)

// liveSession is one entry of `claude agents --json` (and of the
// ~/.claude/sessions registry, which carries the same fields).
type liveSession struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	// Status is busy, waiting or idle.
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"` // unix milliseconds
}

// listWithCLI asks Claude Code for its live sessions. It falls back to the
// sessions registry when the CLI is missing or too old to have `agents --json`.
func (p *Provider) listWithCLI(ctx context.Context) ([]liveSession, error) {
	bin := p.claudePath
	if bin == "" {
		bin = "claude"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "agents", "--json").Output()
	if err == nil {
		var live []liveSession
		if err = json.Unmarshal(out, &live); err == nil {
			return live, nil
		}
	}
	live, regErr := listRegistry()
	if regErr != nil {
		return nil, fmt.Errorf("`%s agents --json`: %v; sessions registry: %w", bin, err, regErr)
	}
	return live, nil
}

// listRegistry reads ~/.claude/sessions/<pid>.json, one file per running
// session. Its format is not a documented interface, so it is only the fallback.
func listRegistry() ([]liveSession, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".claude", "sessions")
	// A missing registry is not "no sessions": without it nothing is known.
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	live := make([]liveSession, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue // the session exited while we listed
		}
		var s liveSession
		if json.Unmarshal(raw, &s) == nil && s.SessionID != "" {
			live = append(live, s)
		}
	}
	return live, nil
}

func stateOf(status string) core.State {
	if status == "busy" {
		return core.StateWorking
	}
	return core.StateSleeping
}

// reconcile reports every live session, catching what hooks miss: sessions
// started before the plugin, turns interrupted with Esc (no Stop fires) and
// sessions that crashed (no SessionEnd fires).
func (p *Provider) reconcile(ctx context.Context) {
	live, err := p.listLive(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logReconcileErr(err)
	if err != nil {
		return // without a reliable list, ending sessions would be a guess
	}
	now := p.now()
	seen := map[string]bool{}
	snapshot := make([]core.Session, 0, len(live))
	for _, l := range live {
		seen[l.SessionID] = true
		s := p.get(l.SessionID)
		if l.Cwd != "" {
			s.info.Cwd = l.Cwd
		}
		if t := titleFor(l.Name, l.Cwd); t != "" {
			s.info.Title = t
		}
		if l.StartedAt > 0 {
			s.info.StartedAt = time.UnixMilli(l.StartedAt)
		}
		// Hooks are exact while fresh; `claude agents` catches what they miss.
		if now.Sub(s.lastHookAt) > hookFresh {
			s.info.State = stateOf(l.Status)
		}
		snapshot = append(snapshot, s.info)
	}
	for id, s := range p.sessions {
		switch {
		case seen[id]:
		case now.Sub(s.lastHookAt) <= hookFresh:
			// Just started: its hooks are ahead of `claude agents`.
			snapshot = append(snapshot, s.info)
		default:
			p.forget(id)
		}
	}
	p.sink.Snapshot(Name, snapshot)
}
