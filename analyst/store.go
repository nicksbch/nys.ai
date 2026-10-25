package main

// Conversations and reports, one JSON file each under the data directory,
// all kept in memory too. Every change rewrites the file (via a temporary
// file and a rename).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"chatapi"
)

// Conversation is one chat: the messages as the model sees them, and the
// full result of every tool call (the model may see a cut version).
type Conversation struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Model    string            `json:"model"`
	Created  time.Time         `json:"created"`
	Updated  time.Time         `json:"updated"`
	Messages []chatapi.Msg     `json:"messages"`
	Results  map[string]Result `json:"results"` // by tool call id
}

type Result struct {
	Text  string `json:"text"`
	Error bool   `json:"error,omitempty"`
}

// Report is a saved analysis: tool calls that run again as they are, with
// no model, on the data as it is then.
type Report struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Notes        string    `json:"notes,omitempty"`
	Conversation string    `json:"conversation,omitempty"` // the one it was saved from
	Created      time.Time `json:"created"`
	Steps        []Step    `json:"steps"`
}

type Step struct {
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"args"`
	Chart json.RawMessage `json:"chart,omitempty"` // how the page shows it; the page's own format
}

type store struct {
	dir   string
	mu    sync.Mutex
	convs map[string]*Conversation
	reps  map[string]*Report
}

func openStore(dir string) (*store, error) {
	s := &store{dir: dir, convs: map[string]*Conversation{}, reps: map[string]*Report{}}
	for _, sub := range []string{"conversations", "reports"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, err
		}
	}
	if err := loadAll(filepath.Join(dir, "conversations"), s.convs); err != nil {
		return nil, err
	}
	if err := loadAll(filepath.Join(dir, "reports"), s.reps); err != nil {
		return nil, err
	}
	return s, nil
}

func loadAll[T any](dir string, into map[string]*T) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		v := new(T)
		if err := json.Unmarshal(b, v); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		into[strings.TrimSuffix(filepath.Base(f), ".json")] = v
	}
	return nil
}

func (s *store) write(sub, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, sub, id+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *store) remove(sub, id string) error {
	err := os.Remove(filepath.Join(s.dir, sub, id+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// conversation returns a copy of a conversation, safe to change and save.
func (s *store) conversation(id string) *Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.convs[id]
	if c == nil {
		return nil
	}
	cp := *c
	cp.Messages = append([]chatapi.Msg(nil), c.Messages...)
	cp.Results = make(map[string]Result, len(c.Results))
	for k, v := range c.Results {
		cp.Results[k] = v
	}
	return &cp
}

func (s *store) saveConversation(c *Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write("conversations", c.ID, c); err != nil {
		return err
	}
	s.convs[c.ID] = c
	return nil
}

func (s *store) deleteConversation(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.convs, id)
	return s.remove("conversations", id)
}

type convSummary struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
}

// conversations lists them, latest first.
func (s *store) conversations() []convSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []convSummary{}
	for _, c := range s.convs {
		out = append(out, convSummary{c.ID, c.Title, c.Updated})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

func (s *store) report(id string) *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reps[id]
}

func (s *store) saveReport(r *Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write("reports", r.ID, r); err != nil {
		return err
	}
	s.reps[r.ID] = r
	return nil
}

func (s *store) deleteReport(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reps, id)
	return s.remove("reports", id)
}

// reports lists them by title.
func (s *store) reports() []*Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*Report{}
	for _, r := range s.reps {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}

// newID returns a prefix and a random hex suffix, safe as a file name.
func newID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
