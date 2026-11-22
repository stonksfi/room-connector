package store

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// ackedRemembered is how long an acknowledged command stays in the log, so a
// pending list fetched just before the ack cannot hand it out a second time.
const ackedRemembered = 10 * time.Minute

// PendingSettle is a command outcome not yet accepted by Room.
type PendingSettle struct {
	CommandID int64  `json:"command_id"`
	Failure   string `json:"failure,omitempty"` // empty means delivered
}

type commandLogFile struct {
	// Dispatched maps each command handed to a provider to when Room accepted
	// its outcome (unix seconds), or 0 while that is outstanding.
	Dispatched map[int64]int64 `json:"dispatched"`
	Settles    []PendingSettle `json:"settles"`
}

// CommandLog remembers which commands roomd handed to an agent and which
// outcomes it still owes Room, across restarts. Without it a restart would
// re-run a message the agent already received.
type CommandLog struct {
	mu   sync.Mutex
	path string
	data commandLogFile
	// inherited holds commands handed out by a previous roomd whose outcome
	// was never reported: their fate is unknown, so they are not re-run.
	inherited map[int64]bool
}

// OpenCommandLog loads the log at path, if any.
func OpenCommandLog(path string) (*CommandLog, error) {
	l := &CommandLog{path: path, data: commandLogFile{Dispatched: map[int64]int64{}}, inherited: map[int64]bool{}}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		// A corrupt log is replaced: the worst case is one re-run message.
		_ = json.Unmarshal(raw, &l.data)
		if l.data.Dispatched == nil {
			l.data.Dispatched = map[int64]int64{}
		}
	}
	settled := map[int64]bool{}
	for _, s := range l.data.Settles {
		settled[s.CommandID] = true
	}
	for id, ackedAt := range l.data.Dispatched {
		if ackedAt == 0 && !settled[id] {
			l.inherited[id] = true
		}
	}
	return l, nil
}

// MarkDispatched records a command handed to a provider. It reports false when
// the command was already handed out, by this roomd or a previous one.
func (l *CommandLog) MarkDispatched(id int64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, seen := l.data.Dispatched[id]; seen {
		return false, nil
	}
	l.data.Dispatched[id] = 0
	return true, l.persist()
}

// Inherited reports whether a previous roomd handed the command out and never
// reported its outcome.
func (l *CommandLog) Inherited(id int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inherited[id]
}

// AddSettle records an outcome to report.
func (l *CommandLog) AddSettle(s PendingSettle) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.data.Dispatched[s.CommandID]; !ok {
		l.data.Dispatched[s.CommandID] = 0
	}
	delete(l.inherited, s.CommandID)
	l.data.Settles = append(l.data.Settles, s)
	return l.persist()
}

// Settles returns the outcomes still owed to Room, oldest first.
func (l *CommandLog) Settles() []PendingSettle {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]PendingSettle(nil), l.data.Settles...)
}

// Acked records that Room accepted (or permanently refused) an outcome, and
// forgets commands acknowledged long enough ago.
func (l *CommandLog) Acked(id int64, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.data.Settles[:0]
	for _, s := range l.data.Settles {
		if s.CommandID != id {
			kept = append(kept, s)
		}
	}
	l.data.Settles = kept
	l.data.Dispatched[id] = now.Unix()
	for other, ackedAt := range l.data.Dispatched {
		if ackedAt != 0 && now.Sub(time.Unix(ackedAt, 0)) > ackedRemembered {
			delete(l.data.Dispatched, other)
		}
	}
	return l.persist()
}

// persist writes the log; the caller holds l.mu.
func (l *CommandLog) persist() error {
	raw, err := json.Marshal(l.data)
	if err != nil {
		return err
	}
	return writeAtomic(l.path, raw)
}
