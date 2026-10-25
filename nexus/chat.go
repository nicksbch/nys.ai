package main

// One turn of a conversation: the user's message, then the model calling
// tools until it answers in text or a limit stops it. Read-only tools run
// right away; a call to any other tool becomes a card, which runs only when
// you press Start. Each step is sent to the page as it happens, and the
// conversation is saved after every step.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	proposeNote  = "\n\nCalling this only proposes the call: the person sees it as a card and starts it, or not."
	proposedText = "Proposed. The person starts it on the page, or not. Say in one sentence what you proposed, and don't call it again."
	// A user message can start with what happened to cards since the last
	// turn, between these markers. The page shows it apart.
	noteStart = "[nexus]\n"
	noteEnd   = "\n[/nexus]\n\n"
)

// event is what the page receives while a turn runs, and also how it reads
// a stored conversation (see items).
type event struct {
	Type  string          `json:"type"` // user, note, text, call, result, card, error, done
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"` // the tool call's
	Name  string          `json:"name,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"`
	Error bool            `json:"error,omitempty"`
	Card  *Card           `json:"card,omitempty"`
}

type turnLimits struct {
	MaxSteps int           // model calls per turn
	Timeout  time.Duration // per turn
}

// turn adds the user's message to c and runs the model until it answers.
// emit receives every step; c is saved as it goes. A turn that fails keeps
// what it did so far, and the error is the turn's last event.
func (a *app) turn(ctx context.Context, c *Conversation, text string, emit func(event)) {
	ctx, cancel := context.WithTimeout(ctx, a.limits.Timeout)
	defer cancel()
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		emit(event{Type: "error", Text: msg})
		// Kept in the conversation, so the page shows it after a reload and
		// the model knows the last turn went wrong.
		c.Messages = append(c.Messages, chatMsg{Role: "assistant", Content: "[nexus: " + msg + "]"})
		a.save(c)
	}

	m := a.cfg.Models[c.Model]
	if m == nil {
		m = a.cfg.Models[a.cfg.Default]
	}
	content := text
	if note := a.takeNotes(c.ID); note != "" {
		emit(event{Type: "note", Text: note})
		content = noteStart + note + noteEnd + text
	}
	c.Messages = append(c.Messages, chatMsg{Role: "user", Content: content})
	if c.Title == "" {
		c.Title = title(text)
	}
	a.save(c)

	sess, err := a.hub.connect(ctx, c.ID)
	if err != nil {
		fail("%v", err)
		return
	}
	defer sess.Close()
	ts, err := a.hub.tools(ctx, sess)
	if err != nil {
		fail("%v", err)
		return
	}

	for step := 1; ; step++ {
		if step > a.limits.MaxSteps {
			fail("stopped after %d steps without an answer; ask again, more simply", a.limits.MaxSteps)
			return
		}
		r, msg, err := complete(ctx, m, a.cfg.Prompt, c.Messages, ts.defs)
		if err != nil {
			if ctx.Err() != nil {
				fail("stopped: the turn took longer than %s", a.limits.Timeout)
			} else {
				fail("%v", err)
			}
			return
		}
		c.Messages = append(c.Messages, *msg)
		if r.Text != "" {
			emit(event{Type: "text", Text: r.Text})
		}
		if len(r.Calls) == 0 {
			if r.Length {
				fail("the answer was cut: the model ran out of output tokens")
				return
			}
			if r.Text == "" {
				fail("the model returned an empty answer")
				return
			}
			a.save(c)
			return
		}
		for _, tc := range r.Calls {
			forModel := a.handleCall(ctx, sess, c, ts, tc, emit)
			c.Messages = append(c.Messages, chatMsg{Role: "tool", ToolCallID: tc.ID, Content: forModel})
		}
		a.save(c)
	}
}

// handleCall runs a read-only tool, or proposes a call to any other as a
// card, and returns what the model is told.
func (a *app) handleCall(ctx context.Context, sess *mcp.ClientSession, c *Conversation, ts *toolSet, tc toolCall, emit func(event)) string {
	t := ts.byName[tc.Name]
	args, err := parseArgs(tc.Args)
	switch {
	case t == nil:
		err = fmt.Errorf("there is no tool named %q", tc.Name)
	case err == nil && !readOnly(t):
		if err = validateArgs(t.InputSchema, args); err != nil {
			err = fmt.Errorf("the arguments don't fit %s's input schema, so nothing was proposed: %v", tc.Name, err)
		}
	}
	if err != nil {
		// Shown as a quiet line, like a read-only call, and never as a card.
		emit(event{Type: "call", ID: tc.ID, Name: tc.Name, Args: tc.Args})
		c.Results[tc.ID] = Result{Text: err.Error(), Error: true}
		emit(event{Type: "result", ID: tc.ID, Text: err.Error(), Error: true})
		return "ERROR: " + err.Error()
	}
	if !readOnly(t) {
		b, _ := json.Marshal(args)
		card := &Card{ID: tc.ID, Tool: tc.Name, Args: b, State: cardProposed}
		if err := a.store.putCard(c.ID, card); err != nil {
			return "ERROR: could not keep the proposal: " + err.Error()
		}
		for _, old := range a.supersede(c.ID, card) {
			emit(event{Type: "card", ID: old.ID, Card: old})
		}
		emit(event{Type: "card", ID: tc.ID, Card: a.store.card(c.ID, tc.ID)})
		return proposedText
	}
	emit(event{Type: "call", ID: tc.ID, Name: tc.Name, Args: tc.Args})
	_, text, isErr := call(ctx, sess, tc.Name, args)
	c.Results[tc.ID] = Result{Text: text, Error: isErr}
	emit(event{Type: "result", ID: tc.ID, Text: text, Error: isErr})
	if isErr {
		return "ERROR: " + cutForModel(text)
	}
	return cutForModel(text)
}

// supersede replaces the conversation's earlier cards that ask for the same
// thing as card and haven't run: proposed or failed, same tool, same URLs
// in the arguments. That is how a reply that corrects a proposal ("put it
// in uk/crime/2023/ instead") leaves one card to start, not two. Cards
// with no URL argument are never replaced. It returns them as they now are.
func (a *app) supersede(conv string, card *Card) []*Card {
	urls := urlArgs(card.Args)
	if urls == "" {
		return nil
	}
	var out []*Card
	for _, old := range a.store.cardsOf(conv) {
		if old.ID == card.ID || old.Tool != card.Tool || (old.State != cardProposed && old.State != cardFailed) || urlArgs(old.Args) != urls {
			continue
		}
		err := a.store.updateCard(conv, old.ID, func(c *Card) error {
			c.State, c.Told = cardSuperseded, true
			return nil
		}, false)
		if err == nil {
			out = append(out, a.store.card(conv, old.ID))
		}
	}
	return out
}

// urlArgs is the http(s) URLs among a call's arguments, sorted and joined,
// or "" when there are none.
func urlArgs(raw json.RawMessage) string {
	var args map[string]any
	json.Unmarshal(raw, &args)
	var urls []string
	for _, v := range args {
		if s, ok := v.(string); ok && (strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")) {
			urls = append(urls, s)
		}
	}
	sort.Strings(urls)
	return strings.Join(urls, " ")
}

// takeNotes describes the cards that ended since the model last heard, and
// marks them told.
func (a *app) takeNotes(conv string) string {
	var ended []*Card
	for _, card := range a.store.cardsOf(conv) {
		if card.ended() && !card.Told {
			ended = append(ended, card)
		}
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].Updated.Before(ended[j].Updated) })
	var lines []string
	for _, card := range ended {
		line := fmt.Sprintf("Your proposed %s %s (call %s): ", card.Tool, card.Args, card.ID)
		switch card.State {
		case cardDone:
			line += "the person started it, and it succeeded: " + oneLine(card.Result, 500)
		case cardFailed:
			line += "the person started it, and it failed: " + oneLine(card.Error, 500)
		case cardCancelled:
			line += "the person cancelled it."
		}
		lines = append(lines, line)
		a.store.updateCard(conv, card.ID, func(c *Card) error { c.Told = true; return nil }, false)
	}
	return strings.Join(lines, "\n")
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func (a *app) save(c *Conversation) {
	c.Updated = time.Now().UTC()
	if err := a.store.saveConversation(c); err != nil {
		a.logf("saving conversation %s: %v", c.ID, err)
	}
}

// items turns a stored conversation into the events the page shows, with
// each tool call's full result and each card as it is now.
func items(c *Conversation, cards map[string]*Card) []event {
	out := []event{}
	for _, m := range c.Messages {
		switch m.Role {
		case "user":
			text := m.Content
			if rest, ok := strings.CutPrefix(text, noteStart); ok {
				if note, after, ok := strings.Cut(rest, noteEnd); ok {
					out = append(out, event{Type: "note", Text: note})
					text = after
				}
			}
			out = append(out, event{Type: "user", Text: text})
		case "assistant":
			if strings.HasPrefix(m.Content, "[nexus: ") {
				out = append(out, event{Type: "error", Text: strings.TrimSuffix(strings.TrimPrefix(m.Content, "[nexus: "), "]")})
				continue
			}
			if t := strings.TrimSpace(m.Content); t != "" {
				out = append(out, event{Type: "text", Text: t})
			}
			for _, tc := range m.ToolCalls {
				if card := cards[tc.ID]; card != nil {
					out = append(out, event{Type: "card", ID: tc.ID, Card: card})
					continue
				}
				out = append(out, event{Type: "call", ID: tc.ID, Name: tc.Function.Name, Args: argsJSON(tc.Function.Arguments)})
				if r, ok := c.Results[tc.ID]; ok {
					out = append(out, event{Type: "result", ID: tc.ID, Text: r.Text, Error: r.Error})
				}
			}
		}
	}
	return out
}

// title is the start of the first message.
func title(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if r := []rune(t); len(r) > 60 {
		t = strings.TrimSpace(string(r[:60])) + "…"
	}
	return t
}
