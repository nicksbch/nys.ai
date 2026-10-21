package main

// The two model APIs behind one interface. A session holds one run's
// conversation in the API's own format and only ever appends to it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

const callTimeout = 3 * time.Minute // per model call

// retryBase is the first wait before retrying an unavailable model (then
// doubled). A variable so tests can shorten it.
var retryBase = 2 * time.Second

// Why a model's turn ended.
const (
	stopTools   = "tools"   // it asked for tool calls
	stopDone    = "done"    // it answered in text
	stopLength  = "length"  // it ran out of output tokens or context
	stopRefused = "refused" // it declined
)

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

type toolResult struct {
	ID      string
	Text    string
	IsError bool
}

type usage struct {
	In        int64 `json:"input_tokens"`
	Out       int64 `json:"output_tokens"`
	CacheRead int64 `json:"cache_read_tokens,omitempty"`
}

// turn is what the model did in one call.
type turn struct {
	Text   string
	Calls  []toolCall
	Stop   string
	Detail string // why, for length and refused
	Model  string // the model that answered (it may be a fallback)
	Usage  usage
	Raw    json.RawMessage // the response as received, for the transcript
}

// session is one run's conversation with a model.
type session interface {
	// next sends either the results of the previous turn's tool calls or a
	// user message (the first one, or a reminder) and returns the next turn.
	next(ctx context.Context, results []toolResult, user string) (*turn, error)
}

// errUnavailable marks errors where the model could not be reached or was
// overloaded: the run didn't really happen, so the trigger can be retried.
var errUnavailable = errors.New("model unavailable")

func newSession(m *ModelConf, system string, tools []toolDef) session {
	if m.API == "anthropic" {
		return newAnthropicSession(m, system, tools)
	}
	return newOpenAISession(m, system, tools)
}

// --- openai: chat completions -------------------------------------------------

type openAISession struct {
	m        *ModelConf
	http     *http.Client
	tools    []map[string]any
	messages []json.RawMessage
}

func newOpenAISession(m *ModelConf, system string, tools []toolDef) *openAISession {
	s := &openAISession{m: m, http: &http.Client{}}
	for _, t := range tools {
		s.tools = append(s.tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.Schema}})
	}
	s.add(map[string]any{"role": "system", "content": system})
	return s
}

func (s *openAISession) add(v any) {
	b, _ := json.Marshal(v)
	s.messages = append(s.messages, b)
}

func (s *openAISession) next(ctx context.Context, results []toolResult, user string) (*turn, error) {
	for _, r := range results {
		text := r.Text
		if r.IsError {
			text = "ERROR: " + text
		}
		s.add(map[string]any{"role": "tool", "tool_call_id": r.ID, "content": text})
	}
	if user != "" {
		s.add(map[string]any{"role": "user", "content": user})
	}
	body, _ := json.Marshal(map[string]any{
		"model": s.m.Model, "messages": s.messages, "tools": s.tools, "max_tokens": s.m.MaxTokens})

	var raw []byte
	var err error
	for attempt := 0; ; attempt++ {
		raw, err = s.post(ctx, body)
		if err == nil || !errors.Is(err, errUnavailable) || attempt == 2 || ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(retryBase << attempt):
		case <-ctx.Done():
		}
	}
	if err != nil {
		return nil, err
	}

	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      json.RawMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Details    struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Choices) == 0 {
		return nil, fmt.Errorf("%w: unreadable response: %.200s", errUnavailable, raw)
	}
	var msg struct {
		Content   string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	json.Unmarshal(resp.Choices[0].Message, &msg)
	s.messages = append(s.messages, resp.Choices[0].Message) // as received

	t := &turn{Text: msg.Content, Model: resp.Model, Raw: raw,
		Usage: usage{In: resp.Usage.Prompt, Out: resp.Usage.Completion, CacheRead: resp.Usage.Details.Cached}}
	for i, c := range msg.ToolCalls {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		t.Calls = append(t.Calls, toolCall{ID: id, Name: c.Function.Name, Args: json.RawMessage(c.Function.Arguments)})
	}
	switch {
	case len(t.Calls) > 0:
		t.Stop = stopTools
	case resp.Choices[0].FinishReason == "length":
		t.Stop, t.Detail = stopLength, "ran out of output tokens (max_tokens)"
	default:
		t.Stop = stopDone
	}
	return t, nil
}

func (s *openAISession) post(ctx context.Context, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.m.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.m.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.m.key)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	switch {
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s: %.300s", errUnavailable, resp.Status, b)
	case resp.StatusCode >= 300:
		// Rejected, e.g. the conversation no longer fits the context.
		return nil, fmt.Errorf("request rejected: %s: %.500s", resp.Status, b)
	}
	return b, nil
}

// --- anthropic: Messages API --------------------------------------------------

type anthropicSession struct {
	m        *ModelConf
	client   anthropic.Client
	system   string
	tools    []anthropic.BetaToolUnionParam
	messages []anthropic.BetaMessageParam
}

// anthropicOptions lets tests point the client at a fake server.
var anthropicOptions []option.RequestOption

func newAnthropicSession(m *ModelConf, system string, tools []toolDef) *anthropicSession {
	opts := append([]option.RequestOption{option.WithAPIKey(m.key), option.WithMaxRetries(2),
		option.WithRequestTimeout(callTimeout)}, anthropicOptions...)
	s := &anthropicSession{m: m, client: anthropic.NewClient(opts...), system: system}
	// Sorted, so the cached prefix is the same on every run.
	sorted := append([]toolDef(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, t := range sorted {
		schema := anthropic.BetaToolInputSchemaParam{ExtraFields: map[string]any{}}
		for k, v := range t.Schema {
			switch k {
			case "type":
			case "properties":
				schema.Properties = v
			case "required":
				for _, r := range asSlice(v) {
					if name, ok := r.(string); ok {
						schema.Required = append(schema.Required, name)
					}
				}
			default:
				schema.ExtraFields[k] = v
			}
		}
		s.tools = append(s.tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name: t.Name, Description: anthropic.String(t.Description), InputSchema: schema}})
	}
	return s
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	if s == nil {
		if ss, ok := v.([]string); ok {
			for _, x := range ss {
				s = append(s, x)
			}
		}
	}
	return s
}

func (s *anthropicSession) next(ctx context.Context, results []toolResult, user string) (*turn, error) {
	var blocks []anthropic.BetaContentBlockParamUnion
	for _, r := range results {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(r.ID, r.Text, r.IsError))
	}
	if user != "" {
		blocks = append(blocks, anthropic.NewBetaTextBlock(user))
	}
	// All tool results of a turn go back in one user message.
	s.messages = append(s.messages, anthropic.NewBetaUserMessage(blocks...))

	params := anthropic.BetaMessageNewParams{
		Model:        anthropic.Model(s.m.Model),
		MaxTokens:    s.m.MaxTokens,
		System:       []anthropic.BetaTextBlockParam{{Text: s.system}},
		Tools:        s.tools,
		Messages:     s.messages,
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(s.m.Effort)},
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if s.m.Fallbacks == nil || *s.m.Fallbacks {
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
	}
	resp, err := s.client.Beta.Messages.New(ctx, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != 429 && apiErr.StatusCode < 500 &&
			apiErr.StatusCode != 401 && apiErr.StatusCode != 403 {
			return nil, fmt.Errorf("request rejected: %v", err)
		}
		return nil, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	s.messages = append(s.messages, resp.ToParam()) // as returned, thinking included

	t := &turn{Model: string(resp.Model), Raw: json.RawMessage(resp.RawJSON()),
		Usage: usage{In: resp.Usage.InputTokens, Out: resp.Usage.OutputTokens, CacheRead: resp.Usage.CacheReadInputTokens}}
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		t.Stop = stopRefused
		t.Detail = strings.TrimSpace(fmt.Sprintf("%s %s", resp.StopDetails.Category, resp.StopDetails.Explanation))
		return t, nil // content is not read on a refusal
	case anthropic.BetaStopReasonMaxTokens:
		t.Stop, t.Detail = stopLength, "ran out of output tokens (max_tokens)"
	case anthropic.BetaStopReasonModelContextWindowExceeded:
		t.Stop, t.Detail = stopLength, "the conversation no longer fits the model's context window"
	case anthropic.BetaStopReasonToolUse:
		t.Stop = stopTools
	default:
		t.Stop = stopDone
	}
	var text []string
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text = append(text, b.Text)
		case anthropic.BetaToolUseBlock:
			t.Calls = append(t.Calls, toolCall{ID: b.ID, Name: b.Name, Args: json.RawMessage(b.JSON.Input.Raw())})
		}
	}
	t.Text = strings.Join(text, "\n")
	if t.Stop == stopTools && len(t.Calls) == 0 {
		t.Stop = stopDone
	}
	return t, nil
}
