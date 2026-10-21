package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Event is one operation reported by an application: a download, an
// ingestion, or any other kind it chooses. Status is "started" when it begins
// (optional), then "ok" or "failed" when it ends; for a failure, Error carries
// the detail as text (a message, output, a stack trace).
type Event struct {
	Time       time.Time `json:"time"`
	App        string    `json:"app"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	RunID      string    `json:"run_id,omitempty"`      // links a "started" event to its result
	Name       string    `json:"name,omitempty"`        // what was processed: a URL, file, table...
	Bytes      int64     `json:"bytes,omitempty"`       // data moved
	Items      int64     `json:"items,omitempty"`       // rows, files, records...
	DurationMS int64     `json:"duration_ms,omitempty"` // how long it took
	Error      string    `json:"error,omitempty"`
	RelayID    uint64    `json:"relay_id,omitempty"` // set when read from relay
}

const (
	statusOK     = "ok"
	statusFailed = "failed"
	statusStart  = "started"
	maxError     = 16 << 10 // longest error detail kept
)

func (e Event) key() string  { return e.App + "/" + e.Kind }
func (e Event) failed() bool { return e.Status == statusFailed }

// finished reports whether the event ends an operation. Only those count as
// runs; "started" events are shown but not counted.
func (e Event) finished() bool { return e.Status != statusStart }

// normalize fills in a missing status: events without one count as failed
// when they carry an error.
func (e *Event) normalize() {
	if e.Status == "" {
		e.Status = statusOK
		if e.Error != "" {
			e.Status = statusFailed
		}
	}
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (e *Event) validate() error {
	if !nameRE.MatchString(e.App) || !nameRE.MatchString(e.Kind) {
		return fmt.Errorf("app and kind are required: 1-64 characters of A-Z a-z 0-9 _ . -")
	}
	if e.RunID != "" && !nameRE.MatchString(e.RunID) {
		return fmt.Errorf("run_id must be 1-64 characters of A-Z a-z 0-9 _ . -")
	}
	if e.Bytes < 0 || e.Items < 0 || e.DurationMS < 0 {
		return fmt.Errorf("bytes, items and duration_ms must not be negative")
	}
	if len(e.Name) > 1024 {
		e.Name = e.Name[:1024]
	}
	e.normalize()
	if e.Status != statusOK && e.Status != statusFailed && e.Status != statusStart {
		return fmt.Errorf(`status must be "started", "ok" or "failed"`)
	}
	if e.Status != statusFailed && e.Error != "" {
		return fmt.Errorf(`error is only allowed with status "failed"`)
	}
	if len(e.Error) > maxError {
		e.Error = e.Error[:maxError] + "\n… (truncated)"
	}
	now := time.Now()
	if e.Time.IsZero() || e.Time.After(now.Add(time.Minute)) {
		e.Time = now
	}
	return nil
}

// Stat aggregates the events of one app/kind over the retention period.
type Stat struct {
	App         string     `json:"app"`
	Kind        string     `json:"kind"`
	Count       int64      `json:"count"`
	Failures    int64      `json:"failures"`
	Bytes       int64      `json:"bytes"`
	Items       int64      `json:"items"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastFailure *time.Time `json:"last_failure,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

func (s *Stat) add(e Event) {
	s.Count++
	s.Bytes += e.Bytes
	s.Items += e.Items
	t := e.Time
	if e.failed() {
		s.Failures++
		if s.LastFailure == nil || t.After(*s.LastFailure) {
			s.LastFailure, s.LastError = &t, e.Error
		}
	} else if s.LastSuccess == nil || t.After(*s.LastSuccess) {
		s.LastSuccess = &t
	}
}

// eventStore keeps the event log in an append-only JSON-lines file, which is
// replayed on startup. Events older than the retention period are dropped
// from the file at startup. The last window of events is also kept in
// memory, for the dashboard and the MCP tools.
type eventStore struct {
	mu        sync.Mutex
	file      *os.File
	retention time.Duration
	window    time.Duration // how far back recent goes
	stats     map[string]*Stat
	recent    []Event // the last window of events, oldest first
	relayID   uint64  // highest relay event ID stored, to skip redeliveries
}

const recentMax = 200000 // most events kept in memory; the oldest go first

// openEventStore opens the log in dir. window is capped at retention.
func openEventStore(dir string, retention, window time.Duration) (*eventStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "events.jsonl")
	s := &eventStore{retention: retention, window: min(window, retention), stats: map[string]*Stat{}}

	var kept []Event
	dropped := 0
	if f, err := os.Open(path); err == nil {
		cutoff := time.Now().Add(-retention)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.Time.Before(cutoff) {
				dropped++
				continue
			}
			e.normalize()
			kept = append(kept, e)
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if dropped > 0 {
		if err := rewrite(path, kept); err != nil {
			return nil, err
		}
	}
	for _, e := range kept {
		s.index(e)
	}
	s.trim()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	s.file = f
	return s, nil
}

// rewrite atomically replaces the log with the given events.
func rewrite(path string, events []Event) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range events {
		enc.Encode(e)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// index updates the in-memory aggregates. Callers hold s.mu (or own s).
func (s *eventStore) index(e Event) {
	st := s.stats[e.key()]
	if st == nil {
		st = &Stat{App: e.App, Kind: e.Kind}
		s.stats[e.key()] = st
	}
	if e.finished() {
		st.add(e)
	}
	s.relayID = max(s.relayID, e.RelayID)
	if time.Since(e.Time) < s.window {
		s.recent = append(s.recent, e)
		// Late reports may arrive out of order; keep the slice sorted.
		for i := len(s.recent) - 1; i > 0 && s.recent[i].Time.Before(s.recent[i-1].Time); i-- {
			s.recent[i], s.recent[i-1] = s.recent[i-1], s.recent[i]
		}
	}
}

func (s *eventStore) add(e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.file.Write(append(b, '\n')); err != nil {
		return err
	}
	s.index(e)
	s.trim()
	return nil
}

func (s *eventStore) trim() {
	cutoff := time.Now().Add(-s.window)
	i := sort.Search(len(s.recent), func(i int) bool { return !s.recent[i].Time.Before(cutoff) })
	i = max(i, len(s.recent)-recentMax)
	if i > 0 {
		s.recent = append([]Event(nil), s.recent[i:]...)
	}
}

// snapshot returns copies of the aggregates (sorted by app, kind) and of the
// events of the window, oldest first.
func (s *eventStore) snapshot() ([]Stat, []Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trim()
	stats := make([]Stat, 0, len(s.stats))
	for _, st := range s.stats {
		stats = append(stats, *st)
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].App != stats[j].App {
			return stats[i].App < stats[j].App
		}
		return stats[i].Kind < stats[j].Kind
	})
	return stats, append([]Event(nil), s.recent...)
}

func (s *eventStore) lastSuccess(key string) *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.stats[key]; st != nil {
		return st.LastSuccess
	}
	return nil
}

func (s *eventStore) lastRelayID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.relayID
}
