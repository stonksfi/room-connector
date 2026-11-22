package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/stonksfi/room-connector/internal/core"
)

const (
	// MaxQueued bounds the offline queue. Past it the oldest events are
	// dropped: a laptop offline for a long time keeps the recent conversation.
	MaxQueued = 5000
	// compactAfter is how many acknowledged lines the journal may carry
	// before it is rewritten without them.
	compactAfter = 2000
)

// Queued is an event with the sequence number the queue gave it.
type Queued struct {
	Seq   int64      `json:"seq"`
	Event core.Event `json:"event"`
}

// Queue holds events not yet accepted by Room, persisted so a restart or an
// offline stretch loses nothing.
//
// It is an append-only journal (queue.jsonl) plus the highest sequence number
// Room accepted (queue.acked). Push appends, so a hook never pays for
// rewriting the backlog; Drop records the acknowledgement and only now and
// then compacts the journal.
type Queue struct {
	mu      sync.Mutex
	path    string
	events  []Queued
	nextSeq int64
	acked   int64
	garbage int // acknowledged lines still in the journal
	journal *os.File
}

// OpenQueue loads the queue persisted at path, if any.
func OpenQueue(path string) (*Queue, error) {
	q := &Queue{path: path, nextSeq: 1}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(q.ackedPath()); err == nil {
		q.acked, _ = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64) // unreadable: replay all
	}
	if err := q.load(); err != nil {
		return nil, err
	}
	journal, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	q.journal = journal
	return q, nil
}

func (q *Queue) ackedPath() string {
	return q.path + ".acked"
}

func (q *Queue) load() error {
	f, err := os.Open(q.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var item Queued
			// A torn or corrupt line (a crash mid-append) is skipped.
			if json.Unmarshal(line, &item) == nil && item.Seq > 0 {
				if item.Seq >= q.nextSeq {
					q.nextSeq = item.Seq + 1
				}
				if item.Seq > q.acked {
					q.events = append(q.events, item)
				} else {
					q.garbage++
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Push appends events to the journal.
func (q *Queue) Push(events ...core.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	var buf strings.Builder
	for _, e := range events {
		item := Queued{Seq: q.nextSeq, Event: e}
		q.nextSeq++
		line, err := json.Marshal(item)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
		q.events = append(q.events, item)
	}
	if over := len(q.events) - MaxQueued; over > 0 {
		log.Printf("event queue full: dropping the %d oldest unsent events", over)
		if err := q.ackLocked(q.events[over-1].Seq); err != nil {
			return err
		}
	}
	_, err := q.journal.WriteString(buf.String())
	return err
}

// Peek returns up to n of the oldest events without removing them.
func (q *Queue) Peek(n int) []Queued {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n > len(q.events) {
		n = len(q.events)
	}
	return append([]Queued(nil), q.events[:n]...)
}

// Drop removes every event up to and including seq, once Room has accepted
// (or permanently refused) them. Dropping by sequence, not by count, keeps a
// concurrent trim from deleting events that were never sent.
func (q *Queue) Drop(seq int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.ackLocked(seq)
}

func (q *Queue) ackLocked(seq int64) error {
	if seq <= q.acked {
		return nil
	}
	kept := q.events[:0]
	for _, item := range q.events {
		if item.Seq > seq {
			kept = append(kept, item)
		} else {
			q.garbage++
		}
	}
	q.events = kept
	q.acked = seq
	if err := writeAtomic(q.ackedPath(), []byte(strconv.FormatInt(seq, 10))); err != nil {
		return err
	}
	if q.garbage >= compactAfter {
		return q.compactLocked()
	}
	return nil
}

// compactLocked rewrites the journal with only the unacknowledged events.
func (q *Queue) compactLocked() error {
	var buf strings.Builder
	for _, item := range q.events {
		line, err := json.Marshal(item)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := writeAtomic(q.path, []byte(buf.String())); err != nil {
		return fmt.Errorf("compact queue: %w", err)
	}
	journal, err := os.OpenFile(q.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	q.journal.Close()
	q.journal = journal
	q.garbage = 0
	return nil
}

// Len is how many events wait.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.events)
}

// Close closes the journal.
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.journal.Close()
}

// writeAtomic replaces a file through a temporary one, readable only by its owner.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
