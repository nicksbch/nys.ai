package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event is one stored event. ID and Time are set by the server; Data is an
// opaque JSON object that relay never reads.
type Event struct {
	ID    uint64          `json:"id"`
	Time  time.Time       `json:"time"`
	Topic string          `json:"topic"`
	Type  string          `json:"type"`
	RunID string          `json:"run_id,omitempty"` // shared by the events of one run
	Cause uint64          `json:"cause,omitempty"`  // ID of the event that led to this one
	Key   string          `json:"key,omitempty"`    // idempotency key
	Data  json.RawMessage `json:"data,omitempty"`
}

var (
	topicRE = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)
	nameRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

const (
	maxTopic = 128
	maxKey   = 128
	maxData  = 64 << 10
	keyTTL   = 24 * time.Hour // how long an idempotency key is remembered
)

func (e *Event) validate() error {
	if len(e.Topic) > maxTopic || !topicRE.MatchString(e.Topic) {
		return errors.New("topic is required: up to 128 characters of dot-separated a-z 0-9 _ - segments, e.g. url-to-bucket.download")
	}
	if !nameRE.MatchString(e.Type) {
		return errors.New("type is required: 1-64 characters of A-Z a-z 0-9 _ . -")
	}
	if e.RunID != "" && !nameRE.MatchString(e.RunID) {
		return errors.New("run_id must be 1-64 characters of A-Z a-z 0-9 _ . -")
	}
	if len(e.Key) > maxKey {
		return errors.New("key must be at most 128 characters")
	}
	if len(e.Data) > maxData {
		return errors.New("data must be at most 64 KB")
	}
	if d := bytes.TrimSpace(e.Data); len(d) == 0 || string(d) == "null" {
		e.Data = nil
	} else if d[0] != '{' {
		return errors.New("data must be a JSON object")
	}
	return nil
}

// filter selects events by topic patterns ("a.b", "a.*" or "*"), types and
// run ID. Empty fields match everything.
type filter struct {
	topics []string
	types  []string
	runID  string
}

func parseFilter(topic, typ, runID string) (filter, error) {
	f := filter{topics: splitList(topic), types: splitList(typ), runID: runID}
	for _, p := range f.topics {
		if p != "*" && !topicRE.MatchString(strings.TrimSuffix(p, ".*")) {
			return f, fmt.Errorf("bad topic filter %q: want a topic, prefix.* or *", p)
		}
	}
	for _, t := range f.types {
		if !nameRE.MatchString(t) {
			return f, fmt.Errorf("bad type %q", t)
		}
	}
	if runID != "" && !nameRE.MatchString(runID) {
		return f, fmt.Errorf("bad run_id %q", runID)
	}
	return f, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (f filter) match(m *meta) bool {
	if f.runID != "" && m.runID != f.runID {
		return false
	}
	if len(f.types) > 0 && !contains(f.types, m.typ) {
		return false
	}
	if len(f.topics) == 0 {
		return true
	}
	for _, p := range f.topics {
		if matchTopic(p, m.topic) {
			return true
		}
	}
	return false
}

func matchTopic(pat, topic string) bool {
	switch {
	case pat == "*":
		return true
	case strings.HasSuffix(pat, ".*"):
		return strings.HasPrefix(topic, pat[:len(pat)-1])
	}
	return pat == topic
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// segment is one day's file of events, one JSON object per line.
type segment struct {
	day  string // UTC, 2006-01-02
	path string
	f    *os.File
	size int64
}

// meta is what is kept in memory for each event; the rest is read from the
// segment when needed.
type meta struct {
	id    uint64
	time  int64 // unix nanoseconds
	topic string
	typ   string
	runID string
	seg   *segment
	off   int64
	n     int32
}

type keyEntry struct {
	id   uint64
	time int64
}

// store is the event log: daily segment files under dir/events, plus an
// in-memory index rebuilt from them on startup.
type store struct {
	mu        sync.RWMutex
	dir       string
	retention time.Duration
	segs      []*segment
	metas     []meta // ordered by id
	keys      map[string]keyEntry
	strs      map[string]string // interned topics and types
	lastID    uint64
	notify    chan struct{} // closed and replaced on every new event
}

const dayFormat = "2006-01-02"

func openStore(dir string, retention time.Duration) (*store, error) {
	s := &store{
		dir:       filepath.Join(dir, "events"),
		retention: retention,
		keys:      map[string]keyEntry{},
		strs:      map[string]string{},
		notify:    make(chan struct{}),
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := s.loadSegment(p); err != nil {
			return nil, err
		}
	}
	s.expire(time.Now())
	return s, nil
}

// loadSegment indexes one segment file. An incomplete last line (a write cut
// short by a crash) is cut off; other unreadable lines are skipped.
func (s *store) loadSegment(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	seg := &segment{day: strings.TrimSuffix(filepath.Base(path), ".jsonl"), path: path, f: f}
	r := bufio.NewReaderSize(f, 64<<10)
	var off int64
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			if len(line) > 0 {
				log.Printf("%s: dropping an incomplete last line (%d bytes)", path, len(line))
				if err := f.Truncate(off); err != nil {
					return err
				}
			}
			break
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		var e Event
		if json.Unmarshal(line, &e) != nil || e.ID <= s.lastID {
			log.Printf("%s: skipping a bad line at offset %d", path, off)
		} else {
			s.index(e, seg, off, len(line))
		}
		off += int64(len(line))
	}
	seg.size = off
	s.segs = append(s.segs, seg)
	return nil
}

func (s *store) intern(v string) string {
	if w, ok := s.strs[v]; ok {
		return w
	}
	s.strs[v] = v
	return v
}

// index adds an event to the in-memory index. Callers hold s.mu (or own s).
func (s *store) index(e Event, seg *segment, off int64, n int) {
	t := e.Time.UnixNano()
	s.metas = append(s.metas, meta{
		id: e.ID, time: t, topic: s.intern(e.Topic), typ: s.intern(e.Type), runID: e.RunID,
		seg: seg, off: off, n: int32(n),
	})
	s.lastID = e.ID
	if e.Key != "" && time.Since(e.Time) < keyTTL {
		s.keys[e.Key] = keyEntry{e.ID, t}
	}
}

// add stores a new event, synced to disk, and returns it with its ID and time
// set. If its key was used in the last 24 hours, add stores nothing and
// returns the original event with dup set.
func (s *store) add(e Event) (stored Event, dup bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Key != "" {
		if k, ok := s.keys[e.Key]; ok && time.Since(time.Unix(0, k.time)) < keyTTL {
			if m := s.find(k.id); m != nil {
				orig, err := s.read(m)
				return orig, true, err
			}
		}
	}
	now := time.Now().UTC()
	e.ID, e.Time = s.lastID+1, now
	seg, err := s.segment(now)
	if err != nil {
		return Event{}, false, err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, false, err
	}
	line = append(line, '\n')
	if _, err := seg.f.Write(line); err != nil {
		seg.f.Truncate(seg.size)
		return Event{}, false, err
	}
	if err := seg.f.Sync(); err != nil {
		return Event{}, false, err
	}
	s.index(e, seg, seg.size, len(line))
	seg.size += int64(len(line))
	close(s.notify)
	s.notify = make(chan struct{})
	return e, false, nil
}

// segment returns the segment to append to at time now, starting a new file
// when the UTC day changes. If the clock goes back, it keeps the current one
// so the files stay in ID order.
func (s *store) segment(now time.Time) (*segment, error) {
	day := now.Format(dayFormat)
	if n := len(s.segs); n > 0 && s.segs[n-1].day >= day {
		return s.segs[n-1], nil
	}
	path := filepath.Join(s.dir, day+".jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	seg := &segment{day: day, path: path, f: f}
	s.segs = append(s.segs, seg)
	return seg, nil
}

// expire deletes whole segments older than the retention period, but always
// keeps the newest so IDs carry on after a long quiet spell, and forgets
// idempotency keys older than 24 hours.
func (s *store) expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-s.retention)
	drop := 0
	for ; drop < len(s.segs)-1; drop++ {
		day, err := time.Parse(dayFormat, s.segs[drop].day)
		if err != nil || !day.Add(24*time.Hour).Before(cutoff) {
			break
		}
	}
	dropped := map[*segment]bool{}
	for _, seg := range s.segs[:drop] {
		dropped[seg] = true
		seg.f.Close()
		if err := os.Remove(seg.path); err != nil {
			log.Printf("retention: %v", err)
		}
	}
	if drop > 0 {
		s.segs = append([]*segment(nil), s.segs[drop:]...)
		i := 0
		for i < len(s.metas) && dropped[s.metas[i].seg] {
			i++
		}
		s.metas = append([]meta(nil), s.metas[i:]...)
		log.Printf("retention: removed %d segment(s) older than %s", drop, cutoff.Format(dayFormat))
	}
	for k, v := range s.keys {
		if now.Sub(time.Unix(0, v.time)) >= keyTTL {
			delete(s.keys, k)
		}
	}
}

func (s *store) find(id uint64) *meta {
	i := sort.Search(len(s.metas), func(i int) bool { return s.metas[i].id >= id })
	if i < len(s.metas) && s.metas[i].id == id {
		return &s.metas[i]
	}
	return nil
}

// read loads an event from its segment. Callers hold s.mu.
func (s *store) read(m *meta) (Event, error) {
	buf := make([]byte, m.n)
	if _, err := m.seg.f.ReadAt(buf, m.off); err != nil {
		return Event{}, err
	}
	var e Event
	err := json.Unmarshal(buf, &e)
	return e, err
}

// after returns up to limit events matching f with IDs above after, and the
// cursor to continue from: the last returned ID if limit was reached,
// otherwise the newest ID scanned. gap reports that events after the cursor
// have already been removed by retention.
func (s *store) after(after uint64, f filter, limit int) (evs []Event, next uint64, gap bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	evs = []Event{}
	gap = len(s.metas) > 0 && after+1 < s.metas[0].id
	next = max(after, s.lastID)
	i := sort.Search(len(s.metas), func(i int) bool { return s.metas[i].id > after })
	for ; i < len(s.metas); i++ {
		m := &s.metas[i]
		if !f.match(m) {
			continue
		}
		e, err := s.read(m)
		if err != nil {
			return nil, 0, false, err
		}
		evs = append(evs, e)
		if len(evs) == limit {
			return evs, e.ID, gap, nil
		}
	}
	return evs, next, gap, nil
}

// last returns the newest n events matching f, oldest first, and the newest ID.
func (s *store) last(n int, f filter) ([]Event, uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	evs := []Event{}
	for i := len(s.metas) - 1; i >= 0 && len(evs) < n; i-- {
		if m := &s.metas[i]; f.match(m) {
			e, err := s.read(m)
			if err != nil {
				return nil, 0, err
			}
			evs = append(evs, e)
		}
	}
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs, s.lastID, nil
}

// count returns how many events matching f have IDs above after.
func (s *store) count(after uint64, f filter) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for i := sort.Search(len(s.metas), func(i int) bool { return s.metas[i].id > after }); i < len(s.metas); i++ {
		if f.match(&s.metas[i]) {
			n++
		}
	}
	return n
}

// changed returns a channel that is closed when the next event is stored.
func (s *store) changed() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.notify
}

func (s *store) latest() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastID
}

type topicInfo struct {
	Topic    string    `json:"topic"`
	Types    []string  `json:"types"`
	Count    int       `json:"count"`
	LastID   uint64    `json:"last_id"`
	LastTime time.Time `json:"last_time"`
}

// topics summarizes every topic in the log, sorted by name.
func (s *store) topics() []topicInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byTopic := map[string]*topicInfo{}
	for i := range s.metas {
		m := &s.metas[i]
		t := byTopic[m.topic]
		if t == nil {
			t = &topicInfo{Topic: m.topic}
			byTopic[m.topic] = t
		}
		if !contains(t.Types, m.typ) {
			t.Types = append(t.Types, m.typ)
		}
		t.Count++
		t.LastID, t.LastTime = m.id, time.Unix(0, m.time).UTC()
	}
	out := make([]topicInfo, 0, len(byTopic))
	for _, t := range byTopic {
		sort.Strings(t.Types)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

func (s *store) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seg := range s.segs {
		seg.f.Close()
	}
}
