package main

// One run: an agent works on a task, step by step, until it submits its
// result or a limit stops it. Every step is written to a recorder.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxToolText  = 16 << 10 // longest tool result the model sees
	submitTool   = "submit"
	reminderText = "Call the submit tool with your result."
)

// runSpec is everything one run needs.
type runSpec struct {
	RunID    string
	Agent    *Agent
	Type     *TaskType
	Model    *ModelConf
	ModelKey string // the model's name in models.json
	Input    string // the first user message: the trigger
	HubURL   string
	HubToken string
}

type runResult struct {
	Status string            `json:"status"` // "ok" or "failed"
	Result map[string]string `json:"result,omitempty"`
	Error  string            `json:"error,omitempty"`
	Steps  int               `json:"steps"`
	Usage  usage             `json:"usage"`
}

// recorder receives the transcript, one record at a time.
type recorder func(kind string, data map[string]any)

// run runs one agent on one task. An error is returned only when the run
// could not take place at all (hub or model unavailable); everything else,
// including the agent failing, is a runResult.
func run(ctx context.Context, spec runSpec, rec recorder) (*runResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, spec.Agent.Timeout)
	defer cancel()

	hub, err := connectHub(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("%w: mcp-hub: %v", errUnavailable, err)
	}
	defer hub.Close()
	offered, withheld, err := hubTools(ctx, hub)
	if err != nil {
		return nil, fmt.Errorf("%w: mcp-hub: %v", errUnavailable, err)
	}
	tools := append(offered, submitDef(spec.Type.Output))
	system := spec.Agent.Prompt + "\n\n" + spec.Type.Prompt + "\n\n" +
		"When you are done, call the submit tool with your result. Calling it ends your work on this task."

	names := make([]string, len(offered))
	for i, t := range offered {
		names[i] = t.Name
	}
	rec("start", map[string]any{"run_id": spec.RunID, "agent": spec.Agent.Name, "task_type": spec.Type.Name,
		"model": spec.ModelKey, "profile": spec.Agent.Profile, "tools": names, "withheld": withheld,
		"system": system, "input": spec.Input})

	res := &runResult{}
	end := func(status, errText string) (*runResult, error) {
		res.Status, res.Error = status, errText
		rec("end", map[string]any{"status": status, "result": res.Result, "error": errText,
			"steps": res.Steps, "usage": res.Usage, "duration_ms": time.Since(start).Milliseconds()})
		return res, nil
	}

	sess := newSession(spec.Model, system, tools)
	var results []toolResult
	user, reminded := spec.Input, false
	for res.Steps < spec.Agent.MaxSteps {
		res.Steps++
		stepStart := time.Now()
		t, err := sess.next(ctx, results, user)
		results, user = nil, ""
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return end("failed", fmt.Sprintf("timed out after %s", spec.Agent.Timeout))
			case errors.Is(err, errUnavailable):
				rec("end", map[string]any{"status": "unavailable", "error": err.Error(), "steps": res.Steps})
				return nil, err
			}
			return end("failed", err.Error())
		}
		res.Usage.In += t.Usage.In
		res.Usage.Out += t.Usage.Out
		res.Usage.CacheRead += t.Usage.CacheRead
		rec("model", map[string]any{"step": res.Steps, "stop": t.Stop, "detail": t.Detail, "text": t.Text,
			"calls": t.Calls, "answered_by": t.Model, "usage": t.Usage,
			"duration_ms": time.Since(stepStart).Milliseconds(), "response": t.Raw})

		switch t.Stop {
		case stopRefused:
			return end("failed", "the model declined: "+t.Detail)
		case stopLength:
			return end("failed", t.Detail)
		case stopDone:
			if reminded {
				return end("failed", "the model stopped without calling submit")
			}
			user, reminded = reminderText, true
			continue
		}

		for _, c := range t.Calls {
			if c.Name == submitTool {
				fields, err := parseSubmit(c.Args, spec.Type.Output)
				if err == nil {
					res.Result = fields
					rec("tool", map[string]any{"step": res.Steps, "name": c.Name, "args": c.Args})
					return end("ok", "")
				}
				results = append(results, toolResult{ID: c.ID, Text: err.Error(), IsError: true})
				rec("tool", map[string]any{"step": res.Steps, "name": c.Name, "args": c.Args, "error": true, "result": err.Error()})
				continue
			}
			results = append(results, callTool(ctx, hub, c, names, res.Steps, rec))
		}
		if ctx.Err() != nil {
			return end("failed", fmt.Sprintf("timed out after %s", spec.Agent.Timeout))
		}
	}
	return end("failed", fmt.Sprintf("reached max_steps (%d) without submitting a result", spec.Agent.MaxSteps))
}

// --- tools --------------------------------------------------------------------

// runIDTransport sends the hub's token and the run id on every request, so
// mcp-hub can link its audit events to this run.
type runIDTransport struct{ token, runID string }

func (t runIDTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	r.Header.Set("X-Run-Id", t.runID)
	return http.DefaultTransport.RoundTrip(r)
}

func connectHub(ctx context.Context, spec runSpec) (*mcp.ClientSession, error) {
	url := strings.TrimRight(spec.HubURL, "/") + "/mcp"
	if spec.Agent.Profile != "all" {
		url += "/" + spec.Agent.Profile
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "foreman", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: runIDTransport{spec.HubToken, spec.RunID}}}, nil)
}

// hubTools lists the profile's tools. Only read-only ones are offered in v1;
// the names of the others are returned as withheld.
func hubTools(ctx context.Context, hub *mcp.ClientSession) (offered []toolDef, withheld []string, err error) {
	withheld = []string{}
	for t, err := range hub.Tools(ctx, nil) {
		if err != nil {
			return nil, nil, err
		}
		if t.Annotations == nil || !t.Annotations.ReadOnlyHint {
			withheld = append(withheld, t.Name)
			continue
		}
		var schema map[string]any
		b, _ := json.Marshal(t.InputSchema)
		json.Unmarshal(b, &schema)
		offered = append(offered, toolDef{Name: t.Name, Description: t.Description, Schema: schema})
	}
	return offered, withheld, nil
}

func callTool(ctx context.Context, hub *mcp.ClientSession, c toolCall, offered []string, step int, rec recorder) toolResult {
	start := time.Now()
	out := toolResult{ID: c.ID}
	var full string
	var args map[string]any
	switch {
	case !slices.Contains(offered, c.Name):
		full, out.IsError = fmt.Sprintf("there is no tool named %q", c.Name), true
	case len(c.Args) > 0 && json.Unmarshal(c.Args, &args) != nil:
		full, out.IsError = "the arguments are not a valid JSON object", true
	default:
		res, err := hub.CallTool(ctx, &mcp.CallToolParams{Name: c.Name, Arguments: args})
		if err != nil {
			full, out.IsError = err.Error(), true
		} else {
			full, out.IsError = resultText(res), res.IsError
		}
	}
	out.Text = cutForModel(full)
	rec("tool", map[string]any{"step": step, "name": c.Name, "args": c.Args, "result": full, "error": out.IsError,
		"cut": len(out.Text) != len(full), "duration_ms": time.Since(start).Milliseconds()})
	return out
}

// resultText is what the model sees of an MCP result: its text, or its
// structured content as JSON when there is no text.
func resultText(res *mcp.CallToolResult) string {
	var s []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			s = append(s, t.Text)
		}
	}
	if len(s) == 0 && res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		return string(b)
	}
	return strings.Join(s, "\n")
}

// cutForModel shortens a long result and says so, so the model knows it
// didn't get everything.
func cutForModel(s string) string {
	if len(s) <= maxToolText {
		return s
	}
	cut := s[:maxToolText]
	for len(cut) > 0 && !utf8Start(s[len(cut)]) {
		cut = cut[:len(cut)-1]
	}
	return fmt.Sprintf("%s\n[cut: %d more bytes]", cut, len(s)-len(cut))
}

// utf8Start reports whether b can begin a UTF-8 character.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// submitDef is the tool that ends a run with the task type's result fields.
func submitDef(fields []string) toolDef {
	props := map[string]any{}
	required := make([]any, len(fields))
	for i, f := range fields {
		props[f] = map[string]any{"type": "string"}
		required[i] = f
	}
	return toolDef{Name: submitTool,
		Description: "Submit your result for this task. Fill in every field. This ends your work on the task.",
		Schema:      map[string]any{"type": "object", "properties": props, "required": required}}
}

func parseSubmit(raw json.RawMessage, fields []string) (map[string]string, error) {
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, fmt.Errorf("submit: the arguments are not a valid JSON object")
	}
	out := map[string]string{}
	var missing []string
	for _, f := range fields {
		s, _ := got[f].(string)
		if strings.TrimSpace(s) == "" {
			missing = append(missing, f)
			continue
		}
		out[f] = s
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("submit: these fields are missing or empty: %s. Call submit again with every field: %s",
			strings.Join(missing, ", "), strings.Join(fields, ", "))
	}
	return out, nil
}
