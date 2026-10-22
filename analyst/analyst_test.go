package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const rows = `{"total_rows": 2, "columns": ["district", "sales"], "records": [{"district": "LEEDS", "sales": 40}, {"district": "YORK", "sales": 5}]}`

// fakeHub is mcp-hub's analyst profile: the semantic layer's tools, and one
// from another server that must not be offered.
type fakeHub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	args []map[string]any // of every query_model call
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{}
	s := mcp.NewServer(&mcp.Implementation{Name: "hub", Version: "1"}, nil)
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"model_name": map[string]any{"type": "string"}, "get_chart": map[string]any{"type": "boolean"},
		"chart_spec": map[string]any{"type": "object"}}}
	s.AddTool(&mcp.Tool{Name: "semantic__query_model", Description: "query", InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			json.Unmarshal(req.Params.Arguments, &args)
			h.mu.Lock()
			h.args = append(h.args, args)
			h.mu.Unlock()
			if args["model_name"] != "price_paid_monthly" {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"error": "Model nope not found"}`}}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: rows}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "vigil__list_jobs", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "jobs"}}}, nil
		})
	mh := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hub-token" || r.URL.Path != "/mcp/analyst" {
			http.Error(w, "unauthorized", 401)
			return
		}
		mh.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// fakeModel answers each request with the next scripted response.
type fakeModel struct {
	srv      *httptest.Server
	mu       sync.Mutex
	script   []string
	requests []map[string]any
}

func newFakeModel(t *testing.T, script ...string) *fakeModel {
	f := &fakeModel{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		resp := f.script[0]
		if len(f.script) > 1 {
			f.script = f.script[1:]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func toolCallResp(name, args string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
		"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "x", "type": "function",
			"function": map[string]any{"name": name, "arguments": args}}}}}}})
	return string(b)
}

func textResp(text string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop",
		"message": map[string]any{"role": "assistant", "content": text}}}})
	return string(b)
}

func newTestApp(t *testing.T, model *fakeModel, hub *fakeHub) (*app, *httptest.Server) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Default: "local", Prompt: "be an analyst",
		Models: map[string]*ModelConf{"local": {URL: model.srv.URL + "/v1", Model: "local", MaxTokens: 100}}}
	a := &app{cfg: cfg, store: st, limits: turnLimits{MaxSteps: 5, Timeout: time.Minute}, logf: t.Logf, busy: map[string]bool{},
		sem: &semantic{url: hub.srv.URL + "/mcp/analyst", token: "hub-token", prefix: "semantic__"}}
	srv := httptest.NewServer(guard("", a.routes()))
	t.Cleanup(srv.Close)
	return a, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Analyst", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// ask sends a question and returns the turn's events.
func ask(t *testing.T, srv *httptest.Server, conv, text string) []event {
	t.Helper()
	return send(t, srv, conv, `{"text":`+mustJSON(text)+`}`)
}

// send posts a message body and returns the turn's events.
func send(t *testing.T, srv *httptest.Server, conv, body string) []event {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/api/conversations/"+conv+"/messages", strings.NewReader(body))
	req.Header.Set("X-Analyst", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var evs []event
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("event %q: %v", sc.Text(), err)
		}
		evs = append(evs, e)
	}
	return evs
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func types(evs []event) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return strings.Join(s, " ")
}

func TestConversationSaveAndRun(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t,
		toolCallResp("semantic__query_model", `{"model_name": "price_paid_monthly", "dimensions": ["district"], "measures": ["sales"], "chart_spec": {"x": 1}}`),
		textResp("Leeds sold most."),
		toolCallResp("vigil__list_jobs", `{}`),
		textResp("I can only query the semantic layer."),
	)
	a, srv := newTestApp(t, model, hub)

	var c struct{ ID string }
	if code := call(t, srv, "POST", "/api/conversations", `{}`, &c); code != 200 {
		t.Fatalf("new conversation: %d", code)
	}
	evs := ask(t, srv, c.ID, "Where were the most sales?")
	if got := types(evs); got != "call result text done" {
		t.Fatalf("events = %s", got)
	}
	if evs[1].Error || evs[1].Text != rows {
		t.Errorf("result = %+v", evs[1])
	}

	// Only the semantic layer's tools, without the chart options.
	model.mu.Lock()
	first := model.requests[0]
	model.mu.Unlock()
	tools := first["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools offered = %v", tools)
	}
	props := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["get_chart"]; ok || props["model_name"] == nil {
		t.Errorf("properties = %v", props)
	}
	hub.mu.Lock()
	sent := hub.args[0]
	hub.mu.Unlock()
	if sent["get_chart"] != false || sent["chart_spec"] != nil {
		t.Errorf("query_model got %v", sent)
	}

	// A follow-up sends the whole conversation; a tool from another server
	// is refused.
	evs = ask(t, srv, c.ID, "And other services?")
	if got := types(evs); got != "call result text done" || !evs[1].Error {
		t.Fatalf("events = %s %+v", got, evs)
	}
	model.mu.Lock()
	msgs := model.requests[2]["messages"].([]any)
	model.mu.Unlock()
	if len(msgs) != 6 { // system, user, assistant (call), tool, assistant, user
		t.Errorf("follow-up sent %d messages: %v", len(msgs), msgs)
	}

	// After a restart, the conversation is read back with every result.
	st, err := openStore(a.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	got := items(st.conversation(c.ID))
	if types(got) != "user call result text user call result text" || got[2].Text != rows {
		t.Errorf("items = %+v", got)
	}

	// Save the first query as a report: its arguments come from the
	// conversation, and it runs again without the model.
	callID := got[1].ID
	var rep Report
	if code := call(t, srv, "POST", "/api/reports", `{"title": "Sales by district", "conversation": "`+c.ID+`",
		"steps": [{"call": "`+callID+`", "chart": {"type": "bar", "measure": "sales"}}]}`, &rep); code != 200 {
		t.Fatalf("save report: %d", code)
	}
	if len(rep.Steps) != 1 || rep.Steps[0].Tool != "semantic__query_model" || !strings.Contains(string(rep.Steps[0].Args), "price_paid_monthly") {
		t.Fatalf("report = %+v", rep)
	}
	if code := call(t, srv, "POST", "/api/reports", `{"title": "x", "conversation": "`+c.ID+`", "steps": [{"call": "`+got[5].ID+`"}]}`, nil); code != 400 {
		t.Errorf("saving a failed call: %d, want 400", code)
	}
	model.mu.Lock()
	before := len(model.requests)
	model.mu.Unlock()
	var out struct{ Results []Result }
	if code := call(t, srv, "POST", "/api/reports/"+rep.ID+"/run", ``, &out); code != 200 || len(out.Results) != 1 || out.Results[0].Text != rows {
		t.Fatalf("run report: %d %+v", code, out)
	}
	model.mu.Lock()
	after := len(model.requests)
	model.mu.Unlock()
	if after != before {
		t.Errorf("running a report called the model")
	}
}

func TestEditQuestion(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t,
		textResp("Leeds."),
		toolCallResp("semantic__query_model", `{"model_name": "price_paid_monthly", "measures": ["sales"]}`),
		textResp("Up."),
		textResp("Flat."),
		textResp("York."),
	)
	a, srv := newTestApp(t, model, hub)
	var c struct{ ID string }
	call(t, srv, "POST", "/api/conversations", `{}`, &c)
	ask(t, srv, c.ID, "Where?")
	ask(t, srv, c.ID, "Trend?")

	// Editing the second question drops it, its call and its answer.
	if got := types(send(t, srv, c.ID, `{"text": "Trend in 2024?", "edit": 1}`)); got != "text done" {
		t.Fatalf("events = %s", got)
	}
	conv := a.store.conversation(c.ID)
	if got := types(items(conv)); got != "user text user text" || len(conv.Results) != 0 || conv.Messages[2].Content != "Trend in 2024?" {
		t.Errorf("after edit: %s, %d results, %+v", got, len(conv.Results), conv.Messages)
	}
	model.mu.Lock()
	msgs := model.requests[len(model.requests)-1]["messages"].([]any)
	model.mu.Unlock()
	if len(msgs) != 4 { // system, user, assistant, the edited user
		t.Errorf("edit sent %d messages: %v", len(msgs), msgs)
	}

	// Editing the first renames the conversation; a question it doesn't
	// have is refused.
	send(t, srv, c.ID, `{"text": "Where in the north?", "edit": 0}`)
	if conv := a.store.conversation(c.ID); conv.Title != "Where in the north?" || types(items(conv)) != "user text" {
		t.Errorf("after editing the first: %q %s", conv.Title, types(items(conv)))
	}
	for _, n := range []string{"1", "-1"} {
		if code := call(t, srv, "POST", "/api/conversations/"+c.ID+"/messages", `{"text": "x", "edit": `+n+`}`, nil); code != 400 {
			t.Errorf("edit %s: %d, want 400", n, code)
		}
	}
}

func TestModelUnreachable(t *testing.T) {
	hub := newFakeHub(t)
	model := newFakeModel(t, textResp("unused"))
	_, srv := newTestApp(t, model, hub)
	model.srv.Close()
	var c struct{ ID string }
	call(t, srv, "POST", "/api/conversations", `{}`, &c)
	evs := ask(t, srv, c.ID, "hello")
	if got := types(evs); got != "error done" || !strings.Contains(evs[0].Text, "unreachable") {
		t.Fatalf("events = %+v", evs)
	}
}

func TestGuard(t *testing.T) {
	h := guard("admin:pw", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, tc := range []struct {
		method, path, user string
		header             bool
		want               int
	}{
		{"GET", "/healthz", "", false, 200},
		{"GET", "/", "", false, 401},
		{"GET", "/", "admin:pw", false, 200},
		{"POST", "/api/reports", "admin:pw", false, 403},
		{"POST", "/api/reports", "admin:pw", true, 200},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.user != "" {
			u, p, _ := strings.Cut(tc.user, ":")
			r.SetBasicAuth(u, p)
		}
		if tc.header {
			r.Header.Set("X-Analyst", "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
}

// The config deploy.sh installs on the server.
func TestRepoConfig(t *testing.T) {
	c, err := loadConfig("config", func(string) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Models[c.Default] == nil || c.Prompt == "" {
		t.Errorf("config = %+v", c)
	}
}

func TestConfigErrors(t *testing.T) {
	for _, tc := range []struct{ models, want string }{
		{`{"default": "a", "models": {}}`, "no models"},
		{`{"default": "b", "models": {"a": {"url": "http://x/v1", "model": "m"}}}`, "default"},
		{`{"default": "a", "models": {"a": {"url": "x", "model": "m"}}}`, "url"},
		{`{"default": "a", "models": {"a": {"url": "http://x/v1", "model": "m", "key_env": "K"}}}`, "K is not set"},
		{`{"default": "a", "models": {"a": {"url": "http://x/v1", "model": "m"}}, "extra": 1}`, "unknown field"},
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "models.json"), []byte(tc.models), 0o644)
		os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("hi"), 0o644)
		if _, err := loadConfig(dir, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.models, err, tc.want)
		}
	}
}

func TestCutAndArgs(t *testing.T) {
	if s := cutForModel(strings.Repeat("é", maxToolText)); !strings.Contains(s, "[cut:") || !json.Valid([]byte(mustJSON(s))) {
		t.Errorf("cut = %.50q", s)
	}
	if string(argsJSON("")) != "{}" || string(argsJSON(`{"a":1}`)) != `{"a":1}` || string(argsJSON(`{bad`)) != `"{bad"` {
		t.Errorf("argsJSON")
	}
}
