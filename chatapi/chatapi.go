// Package chatapi talks to a model through the chat completions API
// (llama-server, or any API that speaks it), for the chat pages analyst and
// nexus. A conversation is kept in that API's message format, so it can be
// stored as is and sent again for a follow-up.
//
// Each app's config directory holds models.json and prompt.md:
//
//	cfg, err := chatapi.LoadConfig("/etc/nexus", os.Getenv, "/etc/nexus.env")
//	r, msg, err := chatapi.Complete(ctx, cfg.Models[cfg.Default], cfg.Prompt, msgs, tools)
package chatapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const callTimeout = 3 * time.Minute // per model call

// Model is one entry of models.json.
type Model struct {
	URL       string `json:"url"` // the API's base URL, e.g. http://127.0.0.1:8081/v1
	Model     string `json:"model"`
	KeyEnv    string `json:"key_env,omitempty"`
	MaxTokens int64  `json:"max_tokens,omitempty"`

	key string // the value of KeyEnv
}

// Config is the config directory: models.json and prompt.md.
type Config struct {
	Default string            `json:"default"` // the model new conversations use
	Models  map[string]*Model `json:"models"`
	Prompt  string            `json:"-"` // the system prompt
}

// LoadConfig reads dir/models.json and dir/prompt.md. Model keys are read
// with getenv; envFile is where the error says to put a missing one.
func LoadConfig(dir string, getenv func(string) string, envFile string) (*Config, error) {
	path := dir + "/models.json"
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Models) == 0 {
		return nil, fmt.Errorf("%s: no models", path)
	}
	if c.Models[c.Default] == nil {
		return nil, fmt.Errorf("%s: default %q is not one of the models", path, c.Default)
	}
	for name, m := range c.Models {
		if err := m.check(getenv, envFile); err != nil {
			return nil, fmt.Errorf("%s: model %q: %w", path, name, err)
		}
	}
	p, err := os.ReadFile(dir + "/prompt.md")
	if err != nil {
		return nil, err
	}
	if c.Prompt = strings.TrimSpace(string(p)); c.Prompt == "" {
		return nil, fmt.Errorf("%s/prompt.md is empty", dir)
	}
	return &c, nil
}

func (m *Model) check(getenv func(string) string, envFile string) error {
	if m.Model == "" {
		return fmt.Errorf("model is required")
	}
	if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
		return fmt.Errorf("url must be the API's base URL, e.g. http://127.0.0.1:8081/v1")
	}
	if m.KeyEnv != "" {
		if m.key = getenv(m.KeyEnv); m.key == "" {
			return fmt.Errorf("environment variable %s is not set (put it in %s)", m.KeyEnv, envFile)
		}
	}
	if m.MaxTokens == 0 {
		m.MaxTokens = 4096
	}
	return nil
}

// ModelNames lists the models, sorted.
func (c *Config) ModelNames() []string {
	var names []string
	for n := range c.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ToolDef is a tool offered to the model.
type ToolDef struct {
	Name        string
	Description string
	Schema      map[string]any // JSON schema of the arguments, type "object"
}

// ToolCall is a call the model made.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// Reply is what the model said in one call.
type Reply struct {
	Text   string
	Calls  []ToolCall
	Length bool // it ran out of output tokens
}

// Msg is a message in the chat completions format. Messages are stored in
// this form, rebuilt from what the model sent rather than as received, so
// tool call ids are always set and unique.
type Msg struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCalls  []Call `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// Call is a tool call in a Msg.
type Call struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ArgsJSON is a tool call's arguments as JSON. Arguments that aren't valid
// JSON are kept as a JSON string, which the tool call then refuses.
func ArgsJSON(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// Complete sends the conversation and returns the model's reply, and the
// reply as the message to add to the conversation.
func Complete(ctx context.Context, m *Model, system string, msgs []Msg, tools []ToolDef) (*Reply, *Msg, error) {
	var apiTools []map[string]any
	for _, t := range tools {
		apiTools = append(apiTools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.Schema}})
	}
	all := append([]Msg{{Role: "system", Content: system}}, msgs...)
	body, _ := json.Marshal(map[string]any{"model": m.Model, "messages": all, "tools": apiTools, "max_tokens": m.MaxTokens})
	raw, err := post(ctx, m, body)
	if err != nil {
		return nil, nil, err
	}
	var resp struct {
		Choices []struct {
			Message      Msg    `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Choices) == 0 {
		return nil, nil, fmt.Errorf("unreadable response from the model: %.200s", raw)
	}
	msg := resp.Choices[0].Message
	msg.Role = "assistant"
	r := &Reply{Text: strings.TrimSpace(msg.Content), Length: resp.Choices[0].FinishReason == "length"}
	for i := range msg.ToolCalls {
		c := &msg.ToolCalls[i]
		c.ID, c.Type = callID(), "function"
		r.Calls = append(r.Calls, ToolCall{ID: c.ID, Name: c.Function.Name, Args: ArgsJSON(c.Function.Arguments)})
	}
	return r, &msg, nil
}

// callID is a new tool call id: "call_" and a random hex suffix.
func callID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}

func post(ctx context.Context, m *Model, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.key != "" {
		req.Header.Set("Authorization", "Bearer "+m.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("the model did not answer within %s", callTimeout)
		}
		return nil, fmt.Errorf("the model is unreachable: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		// e.g. the conversation no longer fits the context
		return nil, fmt.Errorf("the model refused the request: %s: %.500s", resp.Status, b)
	}
	return b, nil
}

// Ping checks that the model's API answers.
func Ping(ctx context.Context, m *Model) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(m.URL, "/")+"/models", nil)
	if err != nil {
		return err
	}
	if m.key != "" {
		req.Header.Set("Authorization", "Bearer "+m.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s/models: %s", m.URL, resp.Status)
	}
	return nil
}

// TestTools asks the model to call a tool once: it shows the API answers
// and takes tools (llama-server needs its chat template for that).
func TestTools(ctx context.Context, m *Model) error {
	ping := ToolDef{Name: "ping", Description: "Answers pong.", Schema: map[string]any{"type": "object", "properties": map[string]any{}}}
	r, _, err := Complete(ctx, m, "You are checking that tool calling works.", []Msg{{Role: "user", Content: "Call the ping tool now."}}, []ToolDef{ping})
	if err != nil {
		return err
	}
	for _, c := range r.Calls {
		if c.Name == "ping" {
			return nil
		}
	}
	return fmt.Errorf("the model answered without calling the tool: %.200s", r.Text)
}
