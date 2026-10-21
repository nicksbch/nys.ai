package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	relay "relay/client"
)

const hubToken = "hub-secret"

func init() {
	retryDown, refreshUp = 50*time.Millisecond, 100*time.Millisecond
}

// fake is an upstream MCP server. While down is set it answers 503.
type fake struct {
	srv       *httptest.Server
	down      atomic.Bool
	lastToken atomic.Value
}

func newFake(t *testing.T, tools ...string) *fake {
	t.Helper()
	f := &fake{}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	for _, name := range tools {
		switch name {
		case "echo": // returns its arguments as text
			s.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
				func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
				})
		case "slow":
			s.AddTool(&mcp.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}},
				func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					select {
					case <-time.After(2 * time.Second):
					case <-ctx.Done():
					}
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "late"}}}, nil
				})
		case "fail":
			s.AddTool(&mcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}},
				func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					res := &mcp.CallToolResult{}
					res.SetError(errorString("download failed: 503"))
					return res, nil
				})
		default:
			s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
				func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name}}}, nil
				})
		}
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		f.lastToken.Store(r.Header.Get("Authorization"))
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type errorString string

func (e errorString) Error() string { return string(e) }

// fakeRelay records the events published to it.
type fakeRelay struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []relay.Event
}

func newFakeRelay(t *testing.T) *fakeRelay {
	r := &fakeRelay{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var e relay.Event
		json.NewDecoder(req.Body).Decode(&e)
		r.mu.Lock()
		r.events = append(r.events, e)
		r.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(e)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRelay) wait(t *testing.T, n int) []relay.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := append([]relay.Event(nil), r.events...)
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("relay got fewer than %d events", n)
	return nil
}

// startHub runs a hub over cfg and returns its HTTP server.
func startHub(t *testing.T, cfg *Config, bus *relay.Client) (*hub, *httptest.Server) {
	t.Helper()
	if err := cfg.check(env(map[string]string{"ALPHA_TOKEN": "alpha-secret"})); err != nil {
		t.Fatal(err)
	}
	h := newHub(cfg, hubToken, bus)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.run(ctx)
	srv := httptest.NewServer(h.routes())
	t.Cleanup(srv.Close)
	return h, srv
}

type authTransport struct{}

func (authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+hubToken)
	r.Header.Set("X-Run-Id", "run-42")
	return http.DefaultTransport.RoundTrip(r)
}

// agent connects to the hub at path, counting list_changed notifications.
func agent(t *testing.T, srv *httptest.Server, path string, changed *atomic.Int32) *mcp.ClientSession {
	t.Helper()
	opts := &mcp.ClientOptions{}
	if changed != nil {
		opts.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) { changed.Add(1) }
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, opts)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL + path, HTTPClient: &http.Client{Transport: authTransport{}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// waitTools waits until the agent sees exactly these tools.
func waitTools(t *testing.T, cs *mcp.ClientSession, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		got = got[:0]
		for _, tool := range res.Tools {
			got = append(got, tool.Name)
		}
		if strings.Join(got, ",") == strings.Join(want, ",") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tools = %v, want %v", got, want)
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return resultText(res), res.IsError
}

func TestRouting(t *testing.T) {
	alpha, beta := newFake(t, "echo", "fail", "slow"), newFake(t, "ping")
	rel := newFakeRelay(t)
	cfg := &Config{
		Servers: []ServerConf{
			{Name: "alpha", URL: alpha.srv.URL, TokenEnv: "ALPHA_TOKEN", Timeout: "200ms"},
			{Name: "beta", URL: beta.srv.URL},
		},
		Profiles: map[string]ProfileConf{
			"ops":  {Servers: []string{"alpha"}, Deny: []string{"alpha__slow"}},
			"ping": {Servers: []string{"alpha", "beta"}, Allow: []string{"beta__*"}},
		},
	}
	_, srv := startHub(t, cfg, &relay.Client{URL: rel.srv.URL})

	all := agent(t, srv, "/mcp", nil)
	waitTools(t, all, "alpha__echo", "alpha__fail", "alpha__slow", "beta__ping")
	waitTools(t, agent(t, srv, "/mcp/ping", nil), "beta__ping")
	ops := agent(t, srv, "/mcp/ops", nil)
	waitTools(t, ops, "alpha__echo", "alpha__fail")

	// Arguments and token reach the upstream unchanged; the result comes back unchanged.
	if text, isErr := callText(t, ops, "alpha__echo", map[string]any{"url": "https://x/a.iso", "n": 2}); isErr || text != `{"n":2,"url":"https://x/a.iso"}` {
		t.Errorf("echo = %q (error %v)", text, isErr)
	}
	if got := alpha.lastToken.Load(); got != "Bearer alpha-secret" {
		t.Errorf("upstream saw Authorization %q", got)
	}
	if text, isErr := callText(t, all, "beta__ping", nil); isErr || text != "ping" {
		t.Errorf("beta__ping = %q (error %v)", text, isErr)
	}
	// An upstream tool error passes through as a tool error.
	if text, isErr := callText(t, ops, "alpha__fail", nil); !isErr || text != "download failed: 503" {
		t.Errorf("fail = %q (error %v)", text, isErr)
	}
	// A slow tool is cut off at the server's timeout.
	if text, isErr := callText(t, all, "alpha__slow", nil); !isErr || text != "alpha did not answer within 200ms" {
		t.Errorf("slow = %q (error %v)", text, isErr)
	}
	// A tool the profile hides doesn't exist there.
	if res, err := ops.CallTool(context.Background(), &mcp.CallToolParams{Name: "alpha__slow"}); err == nil && !res.IsError {
		t.Error("ops profile ran a denied tool")
	}

	events := rel.wait(t, 4)
	byTool := map[string]relay.Event{}
	for _, e := range events {
		if e.Topic != "mcp-hub.call" {
			t.Errorf("topic %q", e.Topic)
		}
		byTool[e.Data.(map[string]any)["name"].(string)] = e
	}
	if byTool["alpha__echo"].RunID != "run-42" {
		t.Errorf("audit run_id = %q, want the caller's X-Run-Id", byTool["alpha__echo"].RunID)
	}
	echo := byTool["alpha__echo"].Data.(map[string]any)
	if byTool["alpha__echo"].Type != "ok" || echo["profile"] != "ops" || echo["server"] != "alpha" || echo["tool"] != "echo" ||
		echo["args"] != `{"n":2,"url":"https://x/a.iso"}` {
		t.Errorf("echo event = %+v", byTool["alpha__echo"])
	}
	fail := byTool["alpha__fail"]
	if fail.Type != "failed" || fail.Data.(map[string]any)["error"] != "download failed: 503" {
		t.Errorf("fail event = %+v", fail)
	}
	slow := byTool["alpha__slow"]
	if slow.Type != "failed" || slow.Data.(map[string]any)["profile"] != "" {
		t.Errorf("slow event = %+v", slow)
	}
}

func TestUpstreamDownAndBack(t *testing.T) {
	alpha, beta := newFake(t, "echo"), newFake(t, "ping")
	alpha.down.Store(true)
	h, srv := startHub(t, &Config{Servers: []ServerConf{
		{Name: "alpha", URL: alpha.srv.URL}, {Name: "beta", URL: beta.srv.URL}}}, nil)

	var changed atomic.Int32
	cs := agent(t, srv, "/mcp", &changed)
	waitTools(t, cs, "beta__ping") // alpha down at startup doesn't stop beta

	if st := h.statuses()[0]; st.State != "down" || st.LastError == "" {
		t.Errorf("alpha status = %+v", st)
	}

	alpha.down.Store(false)
	waitTools(t, cs, "alpha__echo", "beta__ping")
	if changed.Load() == 0 {
		t.Error("agent got no list_changed when alpha came up")
	}

	alpha.down.Store(true)
	waitTools(t, cs, "beta__ping")
	alpha.down.Store(false)
	waitTools(t, cs, "alpha__echo", "beta__ping")
	if text, isErr := callText(t, cs, "alpha__echo", map[string]any{}); isErr || text != "{}" {
		t.Errorf("echo after coming back = %q (error %v)", text, isErr)
	}
}

func TestAuditArgsCutAndRelayDown(t *testing.T) {
	alpha := newFake(t, "echo")
	rel := newFakeRelay(t)
	_, srv := startHub(t, &Config{Servers: []ServerConf{{Name: "alpha", URL: alpha.srv.URL}}}, &relay.Client{URL: rel.srv.URL})
	cs := agent(t, srv, "/mcp", nil)
	waitTools(t, cs, "alpha__echo")
	callText(t, cs, "alpha__echo", map[string]any{"big": strings.Repeat("é", 3000)})
	args := rel.wait(t, 1)[0].Data.(map[string]any)["args"].(string)
	if len(args) > maxAuditText+len("…") || !strings.HasSuffix(args, "…") {
		t.Errorf("args not cut to %d bytes: %d", maxAuditText, len(args))
	}

	// With relay unreachable, calls still work.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	_, srv2 := startHub(t, &Config{Servers: []ServerConf{{Name: "alpha", URL: alpha.srv.URL}}}, &relay.Client{URL: dead.URL})
	cs2 := agent(t, srv2, "/mcp", nil)
	waitTools(t, cs2, "alpha__echo")
	if text, isErr := callText(t, cs2, "alpha__echo", map[string]any{"a": 1}); isErr || text != `{"a":1}` {
		t.Errorf("echo with relay down = %q (error %v)", text, isErr)
	}
}

func TestInvalidToolNamesLeftOut(t *testing.T) {
	alpha := newFake(t, "ok_tool", strings.Repeat("x", 60))
	_, srv := startHub(t, &Config{Servers: []ServerConf{{Name: "alpha", URL: alpha.srv.URL}}}, nil)
	waitTools(t, agent(t, srv, "/mcp", nil), "alpha__ok_tool")
}

func TestHTTP(t *testing.T) {
	alpha := newFake(t, "echo")
	h, srv := startHub(t, &Config{
		Servers:  []ServerConf{{Name: "alpha", URL: alpha.srv.URL}},
		Profiles: map[string]ProfileConf{"ops": {Servers: []string{"alpha"}}},
	}, nil)
	waitTools(t, agent(t, srv, "/mcp", nil), "alpha__echo")

	get := func(path string, auth bool) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer "+hubToken)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/", false); code != 200 || !strings.Contains(body, "alpha") || !strings.Contains(body, "/mcp/ops") {
		t.Errorf("GET / = %d %q", code, body)
	}
	if code, _ := get("/healthz", false); code != 200 {
		t.Errorf("healthz = %d", code)
	}
	for _, p := range []string{"/v1/servers", "/v1/tools"} {
		if code, _ := get(p, false); code != 401 {
			t.Errorf("%s without token = %d", p, code)
		}
	}
	if code, body := get("/v1/tools", true); code != 200 || !strings.Contains(body, `"name":"alpha__echo"`) || !strings.Contains(body, `"profiles":["ops"]`) {
		t.Errorf("/v1/tools = %d %s", code, body)
	}
	if st := h.statuses()[0]; st.State != "up" || st.Tools != 1 {
		t.Errorf("status = %+v", st)
	}

	req, _ := http.NewRequest("POST", srv.URL+"/mcp/ops", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("/mcp/ops with wrong token = %d", resp.StatusCode)
	}
	if code, _ := get("/mcp/nope", true); code != 404 {
		t.Errorf("unknown profile = %d", code)
	}
}

func TestCheck(t *testing.T) {
	alpha := newFake(t, "echo")
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	t.Setenv("ALPHA_TOKEN", "x")
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"servers": [{"name": "alpha", "url": "` + alpha.srv.URL + `", "token_env": "ALPHA_TOKEN"}]}`, 0},
		{`{"servers": [{"name": "alpha", "url": "` + alpha.srv.URL + `"}, {"name": "dead", "url": "` + dead.URL + `"}]}`, 1},
		{`{"servers": [{"name": "alpha", "url": "` + alpha.srv.URL + `", "token_env": "UNSET_TOKEN"}]}`, 2},
	} {
		if got := check([]string{"-config", writeConfig(t, tc.body)}); got != tc.want {
			t.Errorf("check(%s) = %d, want %d", tc.body, got, tc.want)
		}
	}
}
