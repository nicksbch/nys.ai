package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() { retryBase = 10 * time.Millisecond }

// --- fake mcp-hub -------------------------------------------------------------

type fakeHub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	runIDs []string
	calls  []string
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{}
	s := mcp.NewServer(&mcp.Implementation{Name: "hub", Version: "1"}, nil)
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	schema := map[string]any{"type": "object", "properties": map[string]any{"run_id": map[string]any{"type": "string"}}}
	add := func(name string, ann *mcp.ToolAnnotations, text func(args json.RawMessage) string) {
		s.AddTool(&mcp.Tool{Name: name, Description: name + " tool", InputSchema: schema, Annotations: ann},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				h.mu.Lock()
				h.calls = append(h.calls, name)
				h.mu.Unlock()
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text(req.Params.Arguments)}}}, nil
			})
	}
	add("vigil__get_run", ro, func(args json.RawMessage) string { return "run " + string(args) + ": failed 3 times" })
	add("vigil__big", ro, func(json.RawMessage) string { return strings.Repeat("x", 20000) })
	add("url-to-bucket__download", nil, func(json.RawMessage) string { return "downloaded" })
	mh := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hub-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		h.mu.Lock()
		h.runIDs = append(h.runIDs, r.Header.Get("X-Run-Id"))
		h.mu.Unlock()
		mh.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// --- fake model servers -------------------------------------------------------

// fakeModel answers each request with the next scripted response and keeps
// the request bodies and headers.
type fakeModel struct {
	srv      *httptest.Server
	mu       sync.Mutex
	script   []string
	requests []map[string]any
	headers  []http.Header
	delay    time.Duration
	status   int
}

func newFakeModel(t *testing.T, script ...string) *fakeModel {
	f := &fakeModel{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.headers = append(f.headers, r.Header.Clone())
		var resp string
		if len(f.script) > 0 {
			resp = f.script[0]
			if len(f.script) > 1 {
				f.script = f.script[1:]
			}
		}
		delay, status := f.delay, f.status
		f.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status != 0 {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"overloaded"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModel) request(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

// openAI builds a chat-completions response.
func openAI(content string, calls ...[2]string) string {
	msg := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if len(calls) > 0 {
		var tc []map[string]any
		for i, c := range calls {
			tc = append(tc, map[string]any{"id": "call_" + string(rune('a'+i)), "type": "function",
				"function": map[string]any{"name": c[0], "arguments": c[1]}})
		}
		msg["tool_calls"], finish = tc, "tool_calls"
	}
	b, _ := json.Marshal(map[string]any{"model": "gemma", "choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10}})
	return string(b)
}

// anthropicMsg builds a Messages API response.
func anthropicMsg(stop string, content ...map[string]any) string {
	if content == nil {
		content = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
		"content": content, "stop_reason": stop, "stop_sequence": nil, "stop_details": nil,
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "cache_read_input_tokens": 80, "cache_creation_input_tokens": 0}})
	return string(b)
}

func toolUse(id, name string, input any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// --- helpers ------------------------------------------------------------------

type transcript struct {
	mu      sync.Mutex
	records []map[string]any
}

func (tr *transcript) rec(kind string, data map[string]any) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	data["kind"] = kind
	tr.records = append(tr.records, data)
}

func (tr *transcript) kind(kind string) []map[string]any {
	var out []map[string]any
	for _, r := range tr.records {
		if r["kind"] == kind {
			out = append(out, r)
		}
	}
	return out
}

func testSpec(hub *fakeHub, model *ModelConf) runSpec {
	return runSpec{RunID: "run-1",
		Agent: &Agent{Name: "oncall", Profile: "oncall", Model: "m", MaxSteps: 5, Timeout: 10 * time.Second, Prompt: "You are on call."},
		Type:  &TaskType{Name: "duckpipe-failure", Agent: "oncall", Output: []string{"explanation", "fix"}, Prompt: "Explain the failure."},
		Model: model, ModelKey: "m", Input: "Event: {}", HubURL: hub.srv.URL, HubToken: "hub-token"}
}

func openAIConf(f *fakeModel) *ModelConf {
	return &ModelConf{API: "openai", URL: f.srv.URL + "/v1", Model: "gemma", MaxTokens: 100}
}

const goodSubmit = `{"explanation":"N/A in quantity","fix":"clean the file"}`

// --- tests --------------------------------------------------------------------

func TestRunOpenAI(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t,
		openAI("Let me look.", [2]string{"vigil__get_run", `{"run_id":"r1"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	var tr transcript
	res, err := run(context.Background(), testSpec(hub, openAIConf(model)), tr.rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.Steps != 2 || res.Result["explanation"] != "N/A in quantity" || res.Result["fix"] != "clean the file" {
		t.Fatalf("result = %+v", res)
	}
	if res.Usage.In != 200 || res.Usage.Out != 20 {
		t.Errorf("usage = %+v", res.Usage)
	}

	// Only read-only tools are offered, plus submit; the write tool is withheld.
	start := tr.kind("start")[0]
	if strings.Join(start["tools"].([]string), ",") != "vigil__big,vigil__get_run" ||
		strings.Join(start["withheld"].([]string), ",") != "url-to-bucket__download" {
		t.Errorf("start = %v", start)
	}
	var offered []string
	for _, tool := range model.request(0)["tools"].([]any) {
		offered = append(offered, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if strings.Join(offered, ",") != "vigil__big,vigil__get_run,submit" {
		t.Errorf("tools sent = %v", offered)
	}

	// The tool result goes back as a tool message; history is append-only.
	msgs1, msgs2 := model.request(0)["messages"].([]any), model.request(1)["messages"].([]any)
	b1, _ := json.Marshal(msgs1)
	b2, _ := json.Marshal(msgs2[:len(msgs1)])
	if string(b1) != string(b2) {
		t.Error("the second request does not start with the first one's messages")
	}
	last := msgs2[len(msgs2)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "call_a" || last["content"] != `run {"run_id":"r1"}: failed 3 times` {
		t.Errorf("tool message = %v", last)
	}
	if !strings.Contains(msgs1[0].(map[string]any)["content"].(string), "Explain the failure.") {
		t.Error("the system prompt lacks the task type's instructions")
	}

	// The hub saw the run id on every request.
	for _, id := range hub.runIDs {
		if id != "run-1" {
			t.Errorf("hub saw X-Run-Id %q", id)
		}
	}
	if end := tr.kind("end")[0]; end["status"] != "ok" {
		t.Errorf("end = %v", end)
	}
}

func TestRunReminderThenFail(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t, openAI("It failed because of N/A."))
	var tr transcript
	res, _ := run(context.Background(), testSpec(hub, openAIConf(model)), tr.rec)
	if res.Status != "failed" || res.Error != "the model stopped without calling submit" || res.Steps != 2 {
		t.Fatalf("result = %+v", res)
	}
	msgs := model.request(1)["messages"].([]any)
	if last := msgs[len(msgs)-1].(map[string]any); last["role"] != "user" || last["content"] != reminderText {
		t.Errorf("reminder = %v", last)
	}
}

func TestRunReminderThenSubmit(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t, openAI("Done."), openAI("", [2]string{"submit", goodSubmit}))
	res, _ := run(context.Background(), testSpec(hub, openAIConf(model)), (&transcript{}).rec)
	if res.Status != "ok" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunBadSubmit(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t,
		openAI("", [2]string{"submit", `{"explanation":"x"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	res, _ := run(context.Background(), testSpec(hub, openAIConf(model)), (&transcript{}).rec)
	if res.Status != "ok" || res.Steps != 2 {
		t.Fatalf("result = %+v", res)
	}
	msgs := model.request(1)["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if !strings.HasPrefix(last["content"].(string), "ERROR: submit: these fields are missing or empty: fix") {
		t.Errorf("submit error = %v", last["content"])
	}
}

func TestRunMaxSteps(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t, openAI("", [2]string{"vigil__get_run", `{}`}))
	res, _ := run(context.Background(), testSpec(hub, openAIConf(model)), (&transcript{}).rec)
	if res.Status != "failed" || res.Steps != 5 || !strings.Contains(res.Error, "max_steps (5)") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunUnknownToolAndBadArgs(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t,
		openAI("", [2]string{"url-to-bucket__download", `{}`}, [2]string{"vigil__get_run", `{not json`}),
		openAI("", [2]string{"submit", goodSubmit}))
	res, _ := run(context.Background(), testSpec(hub, openAIConf(model)), (&transcript{}).rec)
	if res.Status != "ok" {
		t.Fatalf("result = %+v", res)
	}
	if len(hub.calls) != 0 {
		t.Errorf("the hub was called: %v", hub.calls)
	}
	msgs := model.request(1)["messages"].([]any)
	if c := msgs[len(msgs)-2].(map[string]any)["content"]; c != `ERROR: there is no tool named "url-to-bucket__download"` {
		t.Errorf("withheld tool result = %v", c)
	}
	if c := msgs[len(msgs)-1].(map[string]any)["content"]; c != "ERROR: the arguments are not a valid JSON object" {
		t.Errorf("bad args result = %v", c)
	}
}

func TestRunCutsLongResults(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t, openAI("", [2]string{"vigil__big", `{}`}), openAI("", [2]string{"submit", goodSubmit}))
	var tr transcript
	run(context.Background(), testSpec(hub, openAIConf(model)), tr.rec)
	msgs := model.request(1)["messages"].([]any)
	sent := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if !strings.HasSuffix(sent, "[cut: 3616 more bytes]") || len(sent) > maxToolText+40 {
		t.Errorf("sent %d bytes ending %q", len(sent), sent[len(sent)-30:])
	}
	if tool := tr.kind("tool")[0]; len(tool["result"].(string)) != 20000 || tool["cut"] != true {
		t.Errorf("transcript kept %d bytes, cut %v", len(tool["result"].(string)), tool["cut"])
	}
}

func TestRunTimeout(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t, openAI("slow"))
	model.delay = 2 * time.Second
	spec := testSpec(hub, openAIConf(model))
	spec.Agent.Timeout = 300 * time.Millisecond
	res, err := run(context.Background(), spec, (&transcript{}).rec)
	if err != nil || res.Status != "failed" || res.Error != "timed out after 300ms" {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
}

func TestRunUnavailable(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t, "")
	model.status = 503
	_, err := run(context.Background(), testSpec(hub, openAIConf(model)), (&transcript{}).rec)
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(model.requests) != 3 {
		t.Errorf("%d requests, want 3 (two retries)", len(model.requests))
	}

	// A rejected request (4xx) is the run failing, not the model being down.
	model.status = 400
	res, err := run(context.Background(), testSpec(hub, openAIConf(model)), (&transcript{}).rec)
	if err != nil || res.Status != "failed" || !strings.HasPrefix(res.Error, "request rejected: 400") {
		t.Errorf("400: result = %+v, err = %v", res, err)
	}

	// An unreachable hub means the run can't take place.
	spec := testSpec(hub, openAIConf(model))
	spec.HubToken = "wrong"
	if _, err := run(context.Background(), spec, (&transcript{}).rec); !errors.Is(err, errUnavailable) {
		t.Errorf("hub refused: err = %v", err)
	}
}

func anthropicConf(t *testing.T, f *fakeModel) *ModelConf {
	anthropicOptions = []option.RequestOption{option.WithBaseURL(f.srv.URL), option.WithMaxRetries(0)}
	t.Cleanup(func() { anthropicOptions = nil })
	return &ModelConf{API: "anthropic", Model: "claude-opus-5-5", Effort: "medium", MaxTokens: 16000, key: "k"}
}

func TestRunAnthropic(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t,
		anthropicMsg("tool_use",
			map[string]any{"type": "text", "text": "Checking two things."},
			toolUse("tu_1", "vigil__get_run", map[string]any{"run_id": "r1"}),
			toolUse("tu_2", "vigil__get_run", map[string]any{"run_id": "r2"})),
		anthropicMsg("tool_use", toolUse("tu_3", "submit", map[string]any{"explanation": "N/A in quantity", "fix": "clean the file"})))
	var tr transcript
	res, err := run(context.Background(), testSpec(hub, anthropicConf(t, model)), tr.rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.Result["fix"] != "clean the file" || res.Usage.CacheRead != 160 {
		t.Fatalf("result = %+v", res)
	}

	req := model.request(0)
	if req["fallbacks"] != "default" || !strings.Contains(model.headers[0].Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("fallbacks = %v, beta = %q", req["fallbacks"], model.headers[0].Get("Anthropic-Beta"))
	}
	if req["output_config"].(map[string]any)["effort"] != "medium" || req["cache_control"] == nil {
		t.Errorf("output_config = %v, cache_control = %v", req["output_config"], req["cache_control"])
	}
	if _, ok := req["thinking"]; ok {
		t.Error("thinking must be left unset")
	}
	if _, ok := req["tool_choice"]; ok {
		t.Error("tool_choice must be left unset (auto)")
	}
	var names []string
	for _, tool := range req["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "submit,vigil__big,vigil__get_run" {
		t.Errorf("tools = %v (want sorted)", names)
	}

	// Both results in one user message, after the assistant turn as returned.
	msgs1, msgs2 := req["messages"].([]any), model.request(1)["messages"].([]any)
	if len(msgs2) != len(msgs1)+2 {
		t.Fatalf("messages: %d then %d", len(msgs1), len(msgs2))
	}
	b1, _ := json.Marshal(msgs1)
	b2, _ := json.Marshal(msgs2[:len(msgs1)])
	if string(b1) != string(b2) {
		t.Error("history was edited")
	}
	asst := msgs2[len(msgs1)].(map[string]any)
	if asst["role"] != "assistant" || len(asst["content"].([]any)) != 3 {
		t.Errorf("assistant turn = %v", asst)
	}
	results := msgs2[len(msgs2)-1].(map[string]any)["content"].([]any)
	if len(results) != 2 || results[0].(map[string]any)["tool_use_id"] != "tu_1" || results[1].(map[string]any)["tool_use_id"] != "tu_2" {
		t.Errorf("tool results = %v", results)
	}
	if m := tr.kind("model")[0]; m["answered_by"] != "claude-opus-5-5" || m["text"] != "Checking two things." {
		t.Errorf("model record = %v", m)
	}
}

func TestRunAnthropicRefusal(t *testing.T) {
	hub := newFakeHub(t)
	refusal := strings.Replace(anthropicMsg("refusal"), `"stop_details":null`,
		`"stop_details":{"type":"refusal","category":"cyber","explanation":"not allowed"}`, 1)
	model := newFakeModel(t, refusal)
	res, err := run(context.Background(), testSpec(hub, anthropicConf(t, model)), (&transcript{}).rec)
	if err != nil || res.Status != "failed" || res.Error != "the model declined: cyber not allowed" {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
}

func TestRunAnthropicMaxTokens(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t, anthropicMsg("max_tokens", map[string]any{"type": "text", "text": "and then"}))
	res, _ := run(context.Background(), testSpec(hub, anthropicConf(t, model)), (&transcript{}).rec)
	if res.Status != "failed" || !strings.Contains(res.Error, "max_tokens") {
		t.Fatalf("result = %+v", res)
	}
}

func TestTestModel(t *testing.T) {
	ok := newFakeModel(t, openAI("", [2]string{"ping", `{}`}))
	if err := testModel(context.Background(), openAIConf(ok)); err != nil {
		t.Errorf("tool call: %v", err)
	}
	no := newFakeModel(t, openAI("pong"))
	if err := testModel(context.Background(), openAIConf(no)); err == nil || !strings.Contains(err.Error(), "without calling the tool") {
		t.Errorf("no tool call: %v", err)
	}
}
