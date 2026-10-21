package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "mcp-secret"

// testMonitor returns a monitor holding fixed samples, disks, alerts and
// events, as if it had been running for a while.
func testMonitor(t *testing.T) *monitor {
	t.Helper()
	events, err := openEventStore(t.TempDir(), 90*24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m := &monitor{
		cfg: config{auth: "admin:pw", mcpToken: testToken, interval: 15 * time.Second,
			diskWarn: 85, cpuWarn: 90, memWarn: 90, sustain: 5 * time.Minute,
			expect: map[string]time.Duration{"url-to-bucket/download": 24 * time.Hour, "backup/nightly": 24 * time.Hour}},
		host: "nas", started: now.Add(-48 * time.Hour), events: events, alerts: newAlerter(""),
		disks: []Disk{{Path: "/", Total: 1000, Used: 400, Free: 600, Pct: 40}},
	}
	for i := 0; i < 120; i++ { // the last 2 hours, every minute
		m.samples = append(m.samples, Sample{Time: now.Add(time.Duration(i-119) * time.Minute), CPU: float64(i % 100), Mem: 40, Load1: 0.5})
	}
	m.alerts.set("disk:/data", true, "disk /data is 91% full")

	for _, e := range []Event{
		{Time: now.Add(-9 * 24 * time.Hour), App: "url-to-bucket", Kind: "download", Status: "failed", RunID: "r-9d", Error: "too old"},
		{Time: now.Add(-72 * time.Hour), App: "url-to-bucket", Kind: "download", Status: "failed", RunID: "r0", Error: "GET old.iso: 404"},
		{Time: now.Add(-30 * time.Hour), App: "url-to-bucket", Kind: "download", Status: "ok", Bytes: 7},
		{Time: now.Add(-3 * time.Hour), App: "url-to-bucket", Kind: "download", Status: "started", RunID: "r1", Name: "a.iso"},
		{Time: now.Add(-3*time.Hour + time.Minute), App: "url-to-bucket", Kind: "download", Status: "failed", RunID: "r1",
			Name: "a.iso", Error: "GET a.iso: 503\nline two\nline three"},
		{Time: now.Add(-2 * time.Hour), App: "url-to-bucket", Kind: "download", Status: "started", RunID: "r2", Name: "b.iso"},
		{Time: now.Add(-1 * time.Hour), App: "duckpipe", Kind: "ingest", Status: "ok", RunID: "r3", Items: 10},
	} {
		if err := events.add(e); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, m *monitor) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(m.routes())
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{testToken}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call runs a tool that must succeed and decodes its structured result.
func call[T any](t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) T {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, text(res))
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v in %s", name, err, b)
	}
	return out
}

// callErr runs a tool that must fail and returns its error text.
func callErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("%s %v: want a tool error, got success", name, args)
	}
	return text(res)
}

func text(res *mcp.CallToolResult) string {
	var s []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s = append(s, tc.Text)
		}
	}
	return strings.Join(s, "\n")
}

func TestMCPAuth(t *testing.T) {
	m := testMonitor(t)
	srv := httptest.NewServer(m.routes())
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, tc := range []struct {
		name string
		set  func(*http.Request)
		want int
	}{
		{"no token", func(*http.Request) {}, 401},
		{"wrong token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, 401},
		{"dashboard login", func(r *http.Request) { r.SetBasicAuth("admin", "pw") }, 401},
	} {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		tc.set(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}

	// The MCP token must not open the dashboard.
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("dashboard with MCP token: status %d, want 401", resp.StatusCode)
	}

	// Without -mcp-token, /mcp does not exist.
	m.cfg.mcpToken = ""
	off := httptest.NewServer(m.routes())
	defer off.Close()
	req, _ = http.NewRequest("POST", off.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("no -mcp-token: status %d, want 404", resp.StatusCode)
	}
}

func TestMCPListTools(t *testing.T) {
	cs := connect(t, testMonitor(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"get_system": true, "get_system_history": true, "list_alerts": true,
		"list_jobs": true, "list_events": true, "get_run": true}
	if len(res.Tools) != len(want) {
		t.Errorf("got %d tools, want %d", len(res.Tools), len(want))
	}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
		if strings.Contains(tool.Name, "__") || len("vigil__"+tool.Name) > 64 {
			t.Errorf("%s does not fit mcp-hub's naming rules", tool.Name)
		}
	}
}

func TestMCPSystem(t *testing.T) {
	m := testMonitor(t)
	cs := connect(t, m)
	got := call[systemOut](t, cs, "get_system", nil)
	if got.Host != "nas" || got.CPUPct == nil || *got.CPUPct != 19 || *got.MemPct != 40 {
		t.Errorf("get_system = %+v", got)
	}
	if len(got.Disks) != 1 || got.Disks[0].Pct != 40 || got.Thresholds.DiskPct != 85 || got.Thresholds.Sustain != "5m" {
		t.Errorf("get_system disks/thresholds = %+v %+v", got.Disks, got.Thresholds)
	}

	// No /proc: CPU, memory and load are null.
	m.mu.Lock()
	m.samples = []Sample{{Time: time.Now(), CPU: -1, Mem: -1, Load1: -1}}
	m.mu.Unlock()
	got = call[systemOut](t, cs, "get_system", nil)
	if got.CPUPct != nil || got.MemPct != nil || got.Load1 != nil {
		t.Errorf("unavailable values should be null: %+v", got)
	}
}

func TestMCPSystemHistory(t *testing.T) {
	cs := connect(t, testMonitor(t))
	got := call[historyOut](t, cs, "get_system_history", nil)
	if got.Interval != "15s" || len(got.Samples) != 60 { // 1h of minute samples, default 60 points
		t.Errorf("default: interval %q, %d samples", got.Interval, len(got.Samples))
	}
	got = call[historyOut](t, cs, "get_system_history", map[string]any{"since": "24h", "points": 10})
	if len(got.Samples) != 10 {
		t.Errorf("points=10: %d samples", len(got.Samples))
	}
	for _, args := range []map[string]any{{"since": "25h"}, {"since": "soon"}, {"points": 1}, {"points": 241}} {
		callErr(t, cs, "get_system_history", args)
	}
}

func TestMCPAlerts(t *testing.T) {
	cs := connect(t, testMonitor(t))
	got := call[alertsOut](t, cs, "list_alerts", nil)
	if len(got.Alerts) != 1 || got.Alerts[0].Key != "disk:/data" {
		t.Errorf("list_alerts = %+v", got)
	}
}

func TestMCPJobs(t *testing.T) {
	cs := connect(t, testMonitor(t))
	got := call[jobsOut](t, cs, "list_jobs", nil)
	if len(got.Jobs) != 3 { // backup/nightly is expected but never reported
		t.Fatalf("list_jobs: %d jobs: %+v", len(got.Jobs), got.Jobs)
	}
	dl := got.Jobs[2]
	// Totals cover the 90-day retention, failures_recent the 7 days in memory.
	if dl.App != "url-to-bucket" || dl.Count != 4 || dl.Failures != 3 || dl.FailuresRecent != 2 || dl.Count24 != 1 ||
		dl.Failures24 != 1 || dl.Expect != "24h" || dl.LastError != "GET a.iso: 503 …" || got.Recent != "168h" {
		t.Errorf("url-to-bucket/download = %+v", dl)
	}
	if got.Jobs[0].App != "backup" || got.Jobs[0].Count != 0 {
		t.Errorf("never-reported job = %+v", got.Jobs[0])
	}
	got = call[jobsOut](t, cs, "list_jobs", map[string]any{"app": "duckpipe"})
	if len(got.Jobs) != 1 || got.Jobs[0].Items24 != 10 {
		t.Errorf("app=duckpipe: %+v", got.Jobs)
	}
}

func TestMCPEvents(t *testing.T) {
	cs := connect(t, testMonitor(t))
	got := call[eventsOut](t, cs, "list_events", nil)
	if len(got.Events) != 4 || got.Truncated || got.Events[0].App != "duckpipe" { // newest first; the 30h-old one is gone
		t.Fatalf("list_events = %+v", got)
	}
	if got.Events[2].Error != "GET a.iso: 503 …" {
		t.Errorf("error not cut to one line: %q", got.Events[2].Error)
	}
	for _, tc := range []struct {
		args map[string]any
		n    int
	}{
		{map[string]any{"app": "url-to-bucket"}, 3},
		{map[string]any{"kind": "ingest"}, 1},
		{map[string]any{"status": "failed"}, 1},
		{map[string]any{"since": "150m"}, 2},
		{map[string]any{"since": "48h"}, 5},
		{map[string]any{"since": "168h", "status": "failed"}, 2},
	} {
		if got := call[eventsOut](t, cs, "list_events", tc.args); len(got.Events) != tc.n {
			t.Errorf("%v: %d events, want %d", tc.args, len(got.Events), tc.n)
		}
	}
	got = call[eventsOut](t, cs, "list_events", map[string]any{"limit": 2})
	if len(got.Events) != 2 || !got.Truncated {
		t.Errorf("limit=2: %d events, truncated %v", len(got.Events), got.Truncated)
	}
	for _, args := range []map[string]any{{"status": "running"}, {"since": "169h"}, {"limit": 501}} {
		callErr(t, cs, "list_events", args)
	}
}

func TestMCPRun(t *testing.T) {
	cs := connect(t, testMonitor(t))
	got := call[runOut](t, cs, "get_run", map[string]any{"run_id": "r1"})
	if len(got.Events) != 2 || got.Finished == nil || !*got.Finished || got.RunningFor != "" {
		t.Errorf("r1 = %+v", got)
	}
	if got.Events[1].Error != "GET a.iso: 503\nline two\nline three" {
		t.Errorf("full error not kept: %q", got.Events[1].Error)
	}
	if got := call[runOut](t, cs, "get_run", map[string]any{"run_id": "r0"}); len(got.Events) != 1 {
		t.Errorf("3-day-old run r0 = %+v", got)
	}
	if got := call[runOut](t, cs, "get_run", map[string]any{"run_id": "r-9d"}); len(got.Events) != 0 {
		t.Errorf("run older than the window = %+v", got)
	}
	got = call[runOut](t, cs, "get_run", map[string]any{"run_id": "r2"})
	if got.Finished == nil || *got.Finished || got.RunningFor != "2h00m" {
		t.Errorf("unfinished r2 = %+v", got)
	}
	got = call[runOut](t, cs, "get_run", map[string]any{"run_id": "nope"})
	if len(got.Events) != 0 || got.Finished != nil {
		t.Errorf("unknown run = %+v", got)
	}
	callErr(t, cs, "get_run", map[string]any{})
}
