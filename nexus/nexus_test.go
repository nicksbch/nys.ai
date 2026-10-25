package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	relay "relay/client"
)

func init() { relayRetry = 20 * time.Millisecond }

// fakeHub is mcp-hub's nexus profile: url-to-bucket's two tools.
type fakeHub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []string
	runIDs []string // X-Run-Id of every download call
}

func (h *fakeHub) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c == name {
			n++
		}
	}
	return n
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{}
	s := mcp.NewServer(&mcp.Implementation{Name: "hub", Version: "1"}, nil)
	record := func(name string) {
		h.mu.Lock()
		h.calls = append(h.calls, name)
		h.mu.Unlock()
	}
	s.AddTool(&mcp.Tool{Name: "url-to-bucket__list_buckets", Description: "buckets", InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			record("url-to-bucket__list_buckets")
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"buckets":["home","isos"]}`}}}, nil
		})
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"url": map[string]any{"type": "string"}, "bucket": map[string]any{"type": "string"}, "key": map[string]any{"type": "string"}},
		"required": []any{"url", "bucket"}, "additionalProperties": false}
	n := 0
	s.AddTool(&mcp.Tool{Name: "url-to-bucket__download", Description: "Download a URL.", InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			record("url-to-bucket__download")
			var args map[string]any
			json.Unmarshal(req.Params.Arguments, &args)
			if args["bucket"] == "denied" {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "access denied"}}, IsError: true}, nil
			}
			h.mu.Lock()
			n++
			id := "job-" + strconv.Itoa(n)
			h.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "requested " + id}},
				StructuredContent: map[string]any{"run_id": id, "topic": "url-to-bucket.download"}}, nil
		})
	mh := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hub-token" || r.URL.Path != "/mcp/nexus" {
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

// fakeRelay answers GET /v1/events from a list, filtered as relay does.
type fakeRelay struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []map[string]any
}

func newFakeRelay(t *testing.T) *fakeRelay {
	f := &fakeRelay{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
		types := strings.Split(q.Get("type"), ",")
		f.mu.Lock()
		out := []map[string]any{}
		for _, e := range f.events {
			id := uint64(e["id"].(int))
			if id > after && e["topic"] == q.Get("topic") && e["run_id"] == q.Get("run_id") && contains(types, e["type"].(string)) {
				out = append(out, e)
			}
		}
		f.mu.Unlock()
		if len(out) == 0 {
			time.Sleep(10 * time.Millisecond) // stands in for the long-poll
		}
		json.NewEncoder(w).Encode(map[string]any{"events": out})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeRelay) add(runID, typ string, data map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, map[string]any{"id": len(f.events) + 1, "topic": "url-to-bucket.download", "type": typ,
		"run_id": runID, "data": data})
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

// lastMessages is the messages of the model's latest request.
func (f *fakeModel) lastMessages() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]["messages"].([]any)
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

type env struct {
	a     *app
	srv   *httptest.Server
	hub   *fakeHub
	relay *fakeRelay
	model *fakeModel
	dir   string
}

func newEnv(t *testing.T, script ...string) *env {
	e := &env{hub: newFakeHub(t), relay: newFakeRelay(t), model: newFakeModel(t, script...), dir: t.TempDir()}
	e.a = e.app(t)
	e.srv = httptest.NewServer(guard("", e.a.routes()))
	t.Cleanup(e.srv.Close)
	return e
}

// app opens a nexus on the env's data, as a restart would.
func (e *env) app(t *testing.T) *app {
	st, err := openStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &Config{Default: "local", Prompt: "help",
		Models: map[string]*ModelConf{"local": {URL: e.model.srv.URL + "/v1", Model: "local", MaxTokens: 100}}}
	a := &app{ctx: ctx, cfg: cfg, store: st, limits: turnLimits{MaxSteps: 5, Timeout: time.Minute}, logf: t.Logf, busy: map[string]bool{},
		hub:   &hubClient{url: e.hub.srv.URL + "/mcp/nexus", token: "hub-token"},
		relay: &relay.Client{URL: e.relay.srv.URL, HTTP: &http.Client{}}}
	t.Cleanup(func() { cancel(); a.wg.Wait() })
	return a
}

func (e *env) do(t *testing.T, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Nexus", "1")
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

func (e *env) newConv(t *testing.T) string {
	var c struct{ ID string }
	if code := e.do(t, "POST", "/api/conversations", `{}`, &c); code != 200 {
		t.Fatalf("new conversation: %d", code)
	}
	return c.ID
}

// ask sends a message and returns the turn's events.
func (e *env) ask(t *testing.T, conv, text string) []event {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"text": text})
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/conversations/"+conv+"/messages", strings.NewReader(string(b)))
	req.Header.Set("X-Nexus", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var evs []event
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev event
		json.Unmarshal(sc.Bytes(), &ev)
		evs = append(evs, ev)
	}
	return evs
}

func types(evs []event) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return strings.Join(s, ",")
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// proposed runs a turn that lists the buckets and proposes one download.
func proposed(t *testing.T, e *env, bucket string) (conv string, card *Card) {
	t.Helper()
	conv = e.newConv(t)
	evs := e.ask(t, conv, "get https://x.org/a.iso into "+bucket)
	for _, ev := range evs {
		if ev.Type == "card" {
			card = ev.Card
		}
	}
	if types(evs) != "call,result,card,text,done" || card == nil {
		t.Fatalf("events = %s", types(evs))
	}
	return conv, card
}

func script(bucket string) []string {
	return []string{
		toolCallResp("url-to-bucket__list_buckets", `{}`),
		toolCallResp("url-to-bucket__download", `{"url":"https://x.org/a.iso","bucket":"`+bucket+`"}`),
		textResp("I proposed downloading a.iso into " + bucket + "."),
	}
}

func TestReadOnlyRunsAndOthersAreProposed(t *testing.T) {
	e := newEnv(t, script("isos")...)
	conv, card := proposed(t, e, "isos")
	if card.State != cardProposed || card.Tool != "url-to-bucket__download" || string(card.Args) != `{"bucket":"isos","url":"https://x.org/a.iso"}` {
		t.Errorf("card = %+v", card)
	}
	if e.hub.count("url-to-bucket__list_buckets") != 1 || e.hub.count("url-to-bucket__download") != 0 {
		t.Errorf("hub calls = %v", e.hub.calls)
	}
	// The model was told it is only proposed, and saw the note on the tool.
	msgs := e.model.lastMessages()
	if last := msgs[len(msgs)-1].(map[string]any); last["content"] != proposedText {
		t.Errorf("tool message = %v", last)
	}
	tools, _ := json.Marshal(e.model.requests[0]["tools"])
	if !strings.Contains(string(tools), "only proposes the call") {
		t.Error("the download tool's description lacks the note")
	}
	// The stored conversation shows the same.
	var got struct {
		Items  []event
		Active bool
	}
	e.do(t, "GET", "/api/conversations/"+conv, "", &got)
	if types(got.Items) != "user,call,result,card,text" || got.Items[3].Card.State != cardProposed || got.Active {
		t.Errorf("items = %s, active %v", types(got.Items), got.Active)
	}
}

func TestBadArgumentsMakeNoCard(t *testing.T) {
	e := newEnv(t,
		toolCallResp("url-to-bucket__download", `{"url":"https://x.org/a.iso"}`),
		textResp("Which bucket?"))
	conv := e.newConv(t)
	evs := e.ask(t, conv, "get https://x.org/a.iso")
	if types(evs) != "call,result,text,done" || !evs[1].Error || !strings.Contains(evs[1].Text, "input schema") {
		t.Fatalf("events = %+v", evs)
	}
	if len(e.a.store.cardsOf(conv)) != 0 {
		t.Error("a card was made")
	}
}

func TestStartFollowsTheJob(t *testing.T) {
	e := newEnv(t, append(script("isos"), textResp("It finished: s3://isos/a.iso."))...)
	conv, card := proposed(t, e, "isos")
	path := "/api/conversations/" + conv + "/cards/" + card.ID
	if code := e.do(t, "POST", path+"/start", "", nil); code != 200 {
		t.Fatalf("start: %d", code)
	}
	if code := e.do(t, "POST", path+"/start", "", nil); code != http.StatusConflict {
		t.Errorf("second start: %d", code)
	}
	eventually(t, "running", func() bool { return e.a.store.card(conv, card.ID).State == cardRunning })
	c := e.a.store.card(conv, card.ID)
	if c.JobRunID != "job-1" || c.JobTopic != "url-to-bucket.download" || c.Attempts != 1 {
		t.Errorf("card = %+v", c)
	}
	if e.hub.count("url-to-bucket__download") != 1 {
		t.Errorf("hub calls = %v", e.hub.calls)
	}
	e.hub.mu.Lock()
	sawConv := contains(e.hub.runIDs, conv)
	e.hub.mu.Unlock()
	if !sawConv {
		t.Errorf("the hub never saw the conversation id as X-Run-Id")
	}
	var got struct{ Active bool }
	e.do(t, "GET", "/api/conversations/"+conv, "", &got)
	if !got.Active {
		t.Error("a running card doesn't make the conversation active")
	}

	e.relay.add("job-1", "started", map[string]any{"url": "https://x.org/a.iso"})
	eventually(t, "progress", func() bool { return e.a.store.card(conv, card.ID).Progress == "started" })
	e.relay.add("job-1", "ok", map[string]any{"bucket": "isos", "key": "a.iso", "bytes": 1000})
	eventually(t, "done", func() bool { return e.a.store.card(conv, card.ID).State == cardDone })
	if c := e.a.store.card(conv, card.ID); !strings.Contains(c.Result, `"key":"a.iso"`) {
		t.Errorf("result = %s", c.Result)
	}

	// The next turn tells the model how it ended, once.
	evs := e.ask(t, conv, "did it work?")
	if types(evs) != "note,text,done" || !strings.Contains(evs[0].Text, "succeeded") {
		t.Fatalf("events = %+v", evs)
	}
	msgs := e.model.lastMessages()
	user := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if !strings.HasPrefix(user, noteStart) || !strings.HasSuffix(user, noteEnd+"did it work?") {
		t.Errorf("user message = %q", user)
	}
	if !e.a.store.card(conv, card.ID).Told {
		t.Error("the card isn't marked told")
	}
	var items struct{ Items []event }
	e.do(t, "GET", "/api/conversations/"+conv, "", &items)
	if types(items.Items) != "user,call,result,card,text,note,user,text" || items.Items[6].Text != "did it work?" {
		t.Errorf("items = %s", types(items.Items))
	}
}

func TestJobFailed(t *testing.T) {
	e := newEnv(t, script("isos")...)
	conv, card := proposed(t, e, "isos")
	e.do(t, "POST", "/api/conversations/"+conv+"/cards/"+card.ID+"/start", "", nil)
	eventually(t, "running", func() bool { return e.a.store.card(conv, card.ID).State == cardRunning })
	e.relay.add("job-1", "failed", map[string]any{"error": "HTTP 503"})
	eventually(t, "failed", func() bool { return e.a.store.card(conv, card.ID).State == cardFailed })
	if c := e.a.store.card(conv, card.ID); c.Error != "HTTP 503" {
		t.Errorf("card = %+v", c)
	}
}

func TestFailedCallStartsAgain(t *testing.T) {
	e := newEnv(t, script("denied")...)
	conv, card := proposed(t, e, "denied")
	path := "/api/conversations/" + conv + "/cards/" + card.ID
	e.do(t, "POST", path+"/start", "", nil)
	eventually(t, "failed", func() bool { return e.a.store.card(conv, card.ID).State == cardFailed })
	if c := e.a.store.card(conv, card.ID); c.Error != "access denied" {
		t.Errorf("card = %+v", c)
	}
	if code := e.do(t, "POST", path+"/cancel", "", nil); code != http.StatusConflict {
		t.Errorf("cancel a failed card: %d", code)
	}
	e.do(t, "POST", path+"/start", "", nil)
	eventually(t, "failed again", func() bool {
		c := e.a.store.card(conv, card.ID)
		return c.State == cardFailed && c.Attempts == 2
	})
	if e.hub.count("url-to-bucket__download") != 2 {
		t.Errorf("hub calls = %v", e.hub.calls)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, script("isos")...)
	conv, card := proposed(t, e, "isos")
	path := "/api/conversations/" + conv + "/cards/" + card.ID
	var c Card
	if code := e.do(t, "POST", path+"/cancel", "", &c); code != 200 || c.State != cardCancelled {
		t.Fatalf("cancel: %d %+v", code, c)
	}
	if code := e.do(t, "POST", path+"/start", "", nil); code != http.StatusConflict {
		t.Errorf("start a cancelled card: %d", code)
	}
	if code := e.do(t, "POST", "/api/conversations/"+conv+"/cards/nope/start", "", nil); code != 404 {
		t.Errorf("unknown card: %d", code)
	}
	time.Sleep(30 * time.Millisecond)
	if e.hub.count("url-to-bucket__download") != 0 {
		t.Errorf("hub calls = %v", e.hub.calls)
	}
}

func TestRestart(t *testing.T) {
	e := newEnv(t, textResp("ok"))
	conv := e.newConv(t)
	for _, c := range []*Card{
		{ID: "call_a", Tool: "url-to-bucket__download", Args: json.RawMessage(`{}`), State: cardCalling},
		{ID: "call_b", Tool: "url-to-bucket__download", Args: json.RawMessage(`{}`), State: cardRunning,
			JobRunID: "job-9", JobTopic: "url-to-bucket.download"},
	} {
		if err := e.a.store.putCard(conv, c); err != nil {
			t.Fatal(err)
		}
	}
	e.relay.add("job-9", "ok", map[string]any{"key": "b.iso"})

	a := e.app(t) // nexus starts again on the same data
	a.resume()
	if c := a.store.card(conv, "call_a"); c.State != cardFailed || !strings.Contains(c.Error, "interrupted") {
		t.Errorf("calling card = %+v", c)
	}
	eventually(t, "the running card is followed", func() bool { return a.store.card(conv, "call_b").State == cardDone })
	if e.hub.count("url-to-bucket__download") != 0 {
		t.Error("a card was called again after the restart")
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, textResp("ok"))
	srv := httptest.NewServer(guard("admin:pw", e.a.routes()))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		method, path  string
		login, header bool
		want          int
	}{
		{"GET", "/healthz", false, false, 200},
		{"GET", "/api/conversations", false, false, 401},
		{"GET", "/api/conversations", true, false, 200},
		{"POST", "/api/conversations", true, false, 403},
		{"POST", "/api/conversations", true, true, 200},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(`{}`))
		if tc.login {
			req.SetBasicAuth("admin", "pw")
		}
		if tc.header {
			req.Header.Set("X-Nexus", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s login=%v header=%v: %d, want %d", tc.method, tc.path, tc.login, tc.header, resp.StatusCode, tc.want)
		}
	}
}

func TestConfig(t *testing.T) {
	cfg, err := loadConfig("config", func(string) string { return "" })
	if err != nil || cfg.Models[cfg.Default] == nil || !strings.Contains(cfg.Prompt, "url-to-bucket__download") {
		t.Fatalf("config/: %v", err)
	}
}

func TestReplyReplacesTheProposal(t *testing.T) {
	e := newEnv(t,
		toolCallResp("url-to-bucket__download", `{"url":"https://x.org/a.zip","bucket":"home","key":"uk/crime/a.zip"}`),
		textResp("It will go next to uk/crime/2026-08.zip. Reply with another folder to change it."),
		toolCallResp("url-to-bucket__download", `{"url":"https://x.org/a.zip","bucket":"home","key":"uk/crime/2023/"}`),
		textResp("Changed: uk/crime/2023/."),
		toolCallResp("url-to-bucket__download", `{"url":"https://x.org/b.zip","bucket":"home","key":"uk/crime/2023/"}`),
		textResp("And b.zip too."))
	conv := e.newConv(t)
	first := e.ask(t, conv, "get https://x.org/a.zip")[0].Card
	evs := e.ask(t, conv, "put it in uk/crime/2023")
	// The replaced card comes first, updated, then the new one.
	if types(evs) != "card,card,text,done" || evs[0].Card.ID != first.ID || evs[0].Card.State != cardSuperseded ||
		evs[1].Card.State != cardProposed || !strings.Contains(string(evs[1].Card.Args), "uk/crime/2023/") {
		t.Fatalf("events = %+v", evs)
	}
	if code := e.do(t, "POST", "/api/conversations/"+conv+"/cards/"+first.ID+"/start", "", nil); code != http.StatusConflict {
		t.Errorf("start a replaced card: %d", code)
	}
	// Another URL is another download: nothing is replaced.
	evs = e.ask(t, conv, "and https://x.org/b.zip")
	if types(evs) != "card,text,done" {
		t.Fatalf("events = %s", types(evs))
	}
	states := map[string]int{}
	for _, c := range e.a.store.cardsOf(conv) {
		states[c.State]++
	}
	if states[cardProposed] != 2 || states[cardSuperseded] != 1 {
		t.Errorf("states = %v", states)
	}
	// A replaced card is never reported to the model as ended.
	if note := e.a.takeNotes(conv); note != "" {
		t.Errorf("note = %q", note)
	}
}

func TestDoneCardIsNotReplaced(t *testing.T) {
	e := newEnv(t, textResp("ok"))
	conv := e.newConv(t)
	done := &Card{ID: "call_a", Tool: "url-to-bucket__download", Args: json.RawMessage(`{"url":"https://x.org/a.zip"}`), State: cardDone, Told: true}
	e.a.store.putCard(conv, done)
	e.a.supersede(conv, &Card{ID: "call_b", Tool: "url-to-bucket__download", Args: json.RawMessage(`{"url":"https://x.org/a.zip"}`)})
	if c := e.a.store.card(conv, "call_a"); c.State != cardDone {
		t.Errorf("done card = %+v", c)
	}
}
