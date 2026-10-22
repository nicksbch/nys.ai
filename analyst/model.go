package main

// The model, through the chat completions API (llama-server, or any API
// that speaks it). A conversation is kept in that API's message format, so
// it can be stored as is and sent again for a follow-up.

import (
	"bytes"
	"context"
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

// ModelConf is one entry of models.json.
type ModelConf struct {
	URL       string `json:"url"` // the API's base URL, e.g. http://127.0.0.1:8081/v1
	Model     string `json:"model"`
	KeyEnv    string `json:"key_env,omitempty"`
	MaxTokens int64  `json:"max_tokens,omitempty"`

	key string // the value of KeyEnv
}

// Config is the config directory: models.json and prompt.md.
type Config struct {
	Default string                `json:"default"` // the model new conversations use
	Models  map[string]*ModelConf `json:"models"`
	Prompt  string                `json:"-"` // the system prompt
}

func loadConfig(dir string, getenv func(string) string) (*Config, error) {
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
		if err := m.check(getenv); err != nil {
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

func (m *ModelConf) check(getenv func(string) string) error {
	if m.Model == "" {
		return fmt.Errorf("model is required")
	}
	if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
		return fmt.Errorf("url must be the API's base URL, e.g. http://127.0.0.1:8081/v1")
	}
	if m.KeyEnv != "" {
		if m.key = getenv(m.KeyEnv); m.key == "" {
			return fmt.Errorf("environment variable %s is not set (put it in /etc/analyst.env)", m.KeyEnv)
		}
	}
	if m.MaxTokens == 0 {
		m.MaxTokens = 4096
	}
	return nil
}

func (c *Config) modelNames() []string {
	var names []string
	for n := range c.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

type toolDef struct {
	Name        string
	Description string
	Schema      map[string]any // JSON schema of the arguments, type "object"
}

type toolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// reply is what the model said in one call.
type reply struct {
	Text   string
	Calls  []toolCall
	Length bool // it ran out of output tokens
}

// chatMsg is a message in the chat completions format. Messages are stored
// in this form, rebuilt from what the model sent rather than as received,
// so tool call ids are always set and unique.
type chatMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []chatCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type chatCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// argsJSON is a tool call's arguments as JSON. Arguments that aren't valid
// JSON are kept as a JSON string, which the tool call then refuses.
func argsJSON(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// complete sends the conversation and returns the model's reply.
func complete(ctx context.Context, m *ModelConf, system string, msgs []chatMsg, tools []toolDef) (*reply, *chatMsg, error) {
	var apiTools []map[string]any
	for _, t := range tools {
		apiTools = append(apiTools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.Schema}})
	}
	all := append([]chatMsg{{Role: "system", Content: system}}, msgs...)
	body, _ := json.Marshal(map[string]any{"model": m.Model, "messages": all, "tools": apiTools, "max_tokens": m.MaxTokens})
	raw, err := post(ctx, m, body)
	if err != nil {
		return nil, nil, err
	}
	var resp struct {
		Choices []struct {
			Message      chatMsg `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Choices) == 0 {
		return nil, nil, fmt.Errorf("unreadable response from the model: %.200s", raw)
	}
	msg := resp.Choices[0].Message
	msg.Role = "assistant"
	r := &reply{Text: strings.TrimSpace(msg.Content), Length: resp.Choices[0].FinishReason == "length"}
	for i := range msg.ToolCalls {
		c := &msg.ToolCalls[i]
		c.ID, c.Type = newID("call"), "function"
		r.Calls = append(r.Calls, toolCall{ID: c.ID, Name: c.Function.Name, Args: argsJSON(c.Function.Arguments)})
	}
	return r, &msg, nil
}

func post(ctx context.Context, m *ModelConf, body []byte) ([]byte, error) {
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

// pingModel checks that the model's API answers, for check.
func pingModel(ctx context.Context, m *ModelConf) error {
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
