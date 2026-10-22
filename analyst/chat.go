package main

// One turn of a conversation: the user's message, then the model calling
// the semantic layer's tools until it answers in text or a limit stops it.
// Each step is sent to the page as it happens, and the conversation is
// saved after every step.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// event is what the page receives while a turn runs, and also how it reads
// a stored conversation (see items).
type event struct {
	Type  string          `json:"type"` // user, text, call, result, error, done
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"` // the tool call's
	Name  string          `json:"name,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"`
	Error bool            `json:"error,omitempty"`
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
		c.Messages = append(c.Messages, chatMsg{Role: "assistant", Content: "[analyst: " + msg + "]"})
		a.save(c)
	}

	m := a.cfg.Models[c.Model]
	if m == nil {
		m = a.cfg.Models[a.cfg.Default]
	}
	c.Messages = append(c.Messages, chatMsg{Role: "user", Content: text})
	if c.Title == "" {
		c.Title = title(text)
	}
	a.save(c)

	sess, err := a.sem.connect(ctx)
	if err != nil {
		fail("%v", err)
		return
	}
	defer sess.Close()
	tools, err := a.sem.tools(ctx, sess)
	if err != nil {
		fail("%v", err)
		return
	}

	for step := 1; ; step++ {
		if step > a.limits.MaxSteps {
			fail("stopped after %d steps without an answer; ask again, more narrowly", a.limits.MaxSteps)
			return
		}
		r, msg, err := complete(ctx, m, a.cfg.Prompt, c.Messages, tools)
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
		for _, call := range r.Calls {
			emit(event{Type: "call", ID: call.ID, Name: call.Name, Args: call.Args})
			text, isErr := a.sem.call(ctx, sess, call.Name, call.Args)
			c.Results[call.ID] = Result{Text: text, Error: isErr}
			emit(event{Type: "result", ID: call.ID, Text: text, Error: isErr})
			forModel := cutForModel(text)
			if isErr {
				forModel = "ERROR: " + forModel
			}
			c.Messages = append(c.Messages, chatMsg{Role: "tool", ToolCallID: call.ID, Content: forModel})
		}
		a.save(c)
	}
}

func (a *app) save(c *Conversation) {
	c.Updated = time.Now().UTC()
	if err := a.store.saveConversation(c); err != nil {
		a.logf("saving conversation %s: %v", c.ID, err)
	}
}

// items turns a stored conversation into the events the page shows, with
// each tool call's full result.
func items(c *Conversation) []event {
	out := []event{}
	for _, m := range c.Messages {
		switch m.Role {
		case "user":
			out = append(out, event{Type: "user", Text: m.Content})
		case "assistant":
			if strings.HasPrefix(m.Content, "[analyst: ") {
				out = append(out, event{Type: "error", Text: strings.TrimSuffix(strings.TrimPrefix(m.Content, "[analyst: "), "]")})
				continue
			}
			if t := strings.TrimSpace(m.Content); t != "" {
				out = append(out, event{Type: "text", Text: t})
			}
			for _, tc := range m.ToolCalls {
				out = append(out, event{Type: "call", ID: tc.ID, Name: tc.Function.Name, Args: argsJSON(tc.Function.Arguments)})
				if r, ok := c.Results[tc.ID]; ok {
					out = append(out, event{Type: "result", ID: tc.ID, Text: r.Text, Error: r.Error})
				}
			}
		}
	}
	return out
}

// title is the start of the first question.
func title(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if r := []rune(t); len(r) > 60 {
		t = strings.TrimSpace(string(r[:60])) + "…"
	}
	return t
}
