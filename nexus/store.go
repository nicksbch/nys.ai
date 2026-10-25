package main

// Conversations and their cards, one JSON file each under the data
// directory, all kept in memory too. Every change rewrites the file (via a
// temporary file and a rename). Cards have their own file per conversation,
// because they change while a turn runs (you press Start) and the turn
// saves the conversation from its own copy.

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
)

// Conversation is one chat: the messages as the model sees them, and the
// full result of every tool call that ran during a turn (the model may see a
// cut version).
type Conversation struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Model    string            `json:"model"`
	Created  time.Time         `json:"created"`
	Updated  time.Time         `json:"updated"`
	Messages []chatMsg         `json:"messages"`
	Results  map[string]Result `json:"results"` // by tool call id
}

type Result struct {
	Text  string `json:"text"`
	Error bool   `json:"error,omitempty"`
}

// Card states.
const (
	cardProposed  = "proposed"  // waiting for you
	cardCalling   = "calling"   // you pressed Start; the tool is being called
	cardRunning   = "running"   // a long tool started a job; followed on relay
	cardDone      = "done"      // the call or its job succeeded
	cardFailed    = "failed"    // the call or its job failed; can be started again
	cardCancelled = "cancelled" // you pressed Cancel
)

// Card is a call to a tool that changes something: proposed by the model,
// made only when you press Start.
type Card struct {
	ID       string          `json:"id"` // the tool call's
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
	State    string          `json:"state"`
	Attempts int             `json:"attempts,omitempty"`
	Result   string          `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	Progress string          `json:"progress,omitempty"` // the job's last event, while running
	JobRunID string          `json:"job_run_id,omitempty"`
	JobTopic string          `json:"job_topic,omitempty"`
	Updated  time.Time       `json:"updated"`
	Told     bool            `json:"told,omitempty"` // the model was told how it ended
}

func (c *Card) ended() bool {
	return c.State == cardDone || c.State == cardFailed || c.State == cardCancelled
}

type store struct {
	dir   string
	mu    sync.Mutex
	convs map[string]*Conversation
	cards map[string]map[string]*Card // conversation id -> card id -> card
}

func openStore(dir string) (*store, error) {
	s := &store{dir: dir, convs: map[string]*Conversation{}, cards: map[string]map[string]*Card{}}
	for _, sub := range []string{"conversations", "cards"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, err
		}
	}
	if err := loadAll(filepath.Join(dir, "conversations"), s.convs); err != nil {
		return nil, err
	}
	cards := map[string]*map[string]*Card{}
	if err := loadAll(filepath.Join(dir, "cards"), cards); err != nil {
		return nil, err
	}
	for id, m := range cards {
		s.cards[id] = *m
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
	cp.Messages = append([]chatMsg(nil), c.Messages...)
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
	delete(s.cards, id)
	if err := s.remove("cards", id); err != nil {
		return err
	}
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

// --- cards -----------------------------------------------------------------------

// putCard adds a card to a conversation.
func (s *store) putCard(conv string, c *Card) error {
	return s.updateCard(conv, c.ID, func(old *Card) error {
		*old = *c
		return nil
	}, true)
}

// updateCard changes a card under the lock and saves its conversation's
// cards. f's error leaves everything as it was. Without create, a missing
// card (or conversation) is an error.
func (s *store) updateCard(conv, id string, f func(*Card) error, create bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.convs[conv] == nil {
		return fail(404, "no conversation %q", conv)
	}
	cards := s.cards[conv]
	if cards == nil {
		cards = map[string]*Card{}
	}
	cur := cards[id]
	if cur == nil && !create {
		return fail(404, "no card %q", id)
	}
	next := &Card{}
	if cur != nil {
		*next = *cur
	}
	if err := f(next); err != nil {
		return err
	}
	next.Updated = time.Now().UTC()
	cards[id] = next
	if err := s.write("cards", conv, cards); err != nil {
		if cards[id] = cur; cur == nil {
			delete(cards, id)
		}
		return err
	}
	s.cards[conv] = cards
	return nil
}

// card returns a copy of one card, or nil.
func (s *store) card(conv, id string) *Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cards[conv][id]
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// cardsOf returns copies of a conversation's cards, by id.
func (s *store) cardsOf(conv string) map[string]*Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]*Card{}
	for id, c := range s.cards[conv] {
		cp := *c
		out[id] = &cp
	}
	return out
}

// allCards returns copies of every card, by conversation.
func (s *store) allCards() map[string][]*Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]*Card{}
	for conv, cards := range s.cards {
		for _, c := range cards {
			cp := *c
			out[conv] = append(out[conv], &cp)
		}
	}
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
