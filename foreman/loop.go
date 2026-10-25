package main

// One run: an agent works on a task, step by step, until it submits its
// result or a limit stops it. Every step is written to a recorder.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	mcphub "mcp-hub/client"
)

const (
	maxToolText  = 16 << 10 // longest tool result the model sees
	submitTool   = "submit"
	reminderText = "Call the submit tool with your result."

	defaultActionWait = 30 * time.Minute
	maxActionWait     = 24 * time.Hour
	actionNote        = "\n\nCalling this proposes the action. It doesn't run now. It runs only if a person approves it."
	proposedText      = "Proposed. It will run after approval. Continue, then call submit."
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
	Action *proposal         `json:"action,omitempty"` // kept for the task only if the run ends ok
}

// proposal is an action a run proposed: a call to a tool that changes
// something, made only after you approve it.
type proposal struct {
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args"`
	Preview string          `json:"preview,omitempty"` // what the preview tool said
	WaitMS  int64           `json:"wait_ms"`           // how long a long job may take
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
	ts, err := hubTools(ctx, hub, spec.Type.Actions)
	if err != nil {
		return nil, fmt.Errorf("%w: mcp-hub: %v", errUnavailable, err)
	}
	offered := ts.offered
	tools := append(append([]toolDef{}, offered...), submitDef(spec.Type.Output))
	system := spec.Agent.Prompt + "\n\n" + spec.Type.Prompt + "\n\n" +
		"When you are done, call the submit tool with your result. Calling it ends your work on this task."

	var names, actions []string
	for _, t := range offered {
		if ts.actions[t.Name] != nil {
			actions = append(actions, t.Name)
		} else {
			names = append(names, t.Name)
		}
	}
	rec("start", map[string]any{"run_id": spec.RunID, "agent": spec.Agent.Name, "task_type": spec.Type.Name,
		"model": spec.ModelKey, "profile": spec.Agent.Profile, "tools": names, "actions": actions,
		"withheld": ts.withheld, "system": system, "input": spec.Input})

	res := &runResult{}
	end := func(status, errText string) (*runResult, error) {
		res.Status, res.Error = status, errText
		rec("end", map[string]any{"status": status, "result": res.Result, "action": res.Action, "error": errText,
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
			if t := ts.actions[c.Name]; t != nil {
				results = append(results, propose(ctx, hub, ts, t, c, res, rec))
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

func connectHub(ctx context.Context, spec runSpec) (*mcp.ClientSession, error) {
	url := strings.TrimRight(spec.HubURL, "/") + "/mcp"
	if spec.Agent.Profile != "all" {
		url += "/" + spec.Agent.Profile
	}
	return mcphub.Connect(ctx, "foreman", url, spec.HubToken, spec.RunID)
}

// toolSet is what a run gets of its profile's tools.
type toolSet struct {
	offered  []toolDef            // read-only tools, and the task type's actions
	actions  map[string]*mcp.Tool // offered, but only proposed when called
	withheld []string             // neither: not read-only and not an action
	all      map[string]*mcp.Tool // every tool of the profile, by name
}

// hubTools lists the profile's tools. Read-only ones are offered; tools that
// change something are offered only when the task type lists them in
// actions, and calling them proposes the call. The others are withheld.
func hubTools(ctx context.Context, hub *mcp.ClientSession, actions []string) (*toolSet, error) {
	ts := &toolSet{actions: map[string]*mcp.Tool{}, withheld: []string{}, all: map[string]*mcp.Tool{}}
	for t, err := range hub.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		ts.all[t.Name] = t
		schema := mcphub.Schema(t)
		switch {
		case mcphub.ReadOnly(t):
			ts.offered = append(ts.offered, toolDef{Name: t.Name, Description: t.Description, Schema: schema})
		case slices.Contains(actions, t.Name):
			ts.actions[t.Name] = t
			ts.offered = append(ts.offered, toolDef{Name: t.Name, Description: t.Description + actionNote, Schema: schema})
		default:
			ts.withheld = append(ts.withheld, t.Name)
		}
	}
	return ts, nil
}

// previewName is the hub name of a tool's preview: a read-only tool on the
// same server, named in its _meta, that checks the same arguments without
// acting. "" when it has none.
func previewName(t *mcp.Tool) string {
	p, _ := t.Meta["preview"].(string)
	server, _, ok := strings.Cut(t.Name, "__")
	if p == "" || !ok {
		return ""
	}
	return server + "__" + p
}

// actionWait is how long foreman follows a long tool's job on relay: its
// _meta wait, or the default.
func actionWait(t *mcp.Tool) time.Duration {
	if s, _ := t.Meta["wait"].(string); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return min(d, maxActionWait)
		}
	}
	return defaultActionWait
}

// propose handles a call to an action tool: the call isn't made, but checked
// and stored as the run's proposal. Errors go back to the model to correct.
func propose(ctx context.Context, hub *mcp.ClientSession, ts *toolSet, t *mcp.Tool, c toolCall, res *runResult, rec recorder) toolResult {
	start := time.Now()
	out := toolResult{ID: c.ID, Text: proposedText}
	p, err := checkProposal(ctx, hub, ts, t, c.Args, res.Action != nil)
	if err != nil {
		out.Text, out.IsError = err.Error(), true
	} else {
		res.Action = p
	}
	r := map[string]any{"step": res.Steps, "name": c.Name, "args": c.Args, "result": out.Text, "error": out.IsError,
		"proposed": !out.IsError, "duration_ms": time.Since(start).Milliseconds()}
	if p != nil && p.Preview != "" {
		r["preview"] = p.Preview
	}
	rec("tool", r)
	return out
}

func checkProposal(ctx context.Context, hub *mcp.ClientSession, ts *toolSet, t *mcp.Tool, raw json.RawMessage, already bool) (*proposal, error) {
	if already {
		return nil, errors.New("this run already proposed an action; a run proposes at most one")
	}
	args := map[string]any{}
	if len(raw) > 0 && json.Unmarshal(raw, &args) != nil {
		return nil, errors.New("the arguments are not a valid JSON object")
	}
	if err := mcphub.ValidateArgs(t.InputSchema, args); err != nil {
		return nil, fmt.Errorf("the arguments don't fit %s's input schema: %v", t.Name, err)
	}
	b, _ := json.Marshal(args)
	p := &proposal{Tool: t.Name, Args: b, WaitMS: actionWait(t).Milliseconds()}
	name := previewName(t)
	if name == "" {
		return p, nil
	}
	pt := ts.all[name]
	switch {
	case pt == nil:
		return nil, fmt.Errorf("can't propose %s: its preview tool %s is not in this profile", t.Name, name)
	case !mcphub.ReadOnly(pt):
		return nil, fmt.Errorf("can't propose %s: its preview tool %s isn't read-only", t.Name, name)
	}
	r, err := hub.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("the check before proposing (%s) could not run: %v", name, err)
	}
	if r.IsError {
		return nil, fmt.Errorf("the check before proposing (%s) failed, so nothing was proposed: %s", name, mcphub.ResultText(r))
	}
	p.Preview = mcphub.ResultText(r)
	return p, nil
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
			full, out.IsError = mcphub.ResultText(res), res.IsError
		}
	}
	out.Text = mcphub.CutForModel(full, maxToolText, "")
	rec("tool", map[string]any{"step": step, "name": c.Name, "args": c.Args, "result": full, "error": out.IsError,
		"cut": len(out.Text) != len(full), "duration_ms": time.Since(start).Milliseconds()})
	return out
}

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
