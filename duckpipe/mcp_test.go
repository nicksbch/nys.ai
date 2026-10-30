package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type tokenTransport struct{ token string }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, e *engine, token string) (*mcp.ClientSession, error) {
	t.Helper()
	srv := httptest.NewServer(e.handler("admin:pw", "mcp-token"))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: tokenTransport{token}}, DisableStandaloneSSE: true}, nil)
}

// call calls a tool and decodes its structured result into out.
func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, out)
	}
	return res
}

func TestMCPTools(t *testing.T) {
	e := testEngine(t)
	s, err := connect(t, e, "mcp-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tools, err := s.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	for _, x := range tools.Tools {
		readOnly := x.Annotations != nil && x.Annotations.ReadOnlyHint
		if want := x.Name == "list_pipes" || x.Name == "load_history"; readOnly != want {
			t.Errorf("%s: read-only %v, want %v", x.Name, readOnly, want)
		}
	}

	var v stateView
	call(t, s, "list_pipes", nil, &v)
	if v.State != "idle" || len(v.Pipes) != 1 || v.Pipes[0].Source != "s3://home/raw/orders/*.csv" || v.Pipes[0].Table != "lake.raw.orders" {
		t.Errorf("list_pipes = %+v", v)
	}

	var d doneOut
	call(t, s, "stop_all", nil, &d)
	if d.State != "stopped" {
		t.Errorf("stop_all: state %q", d.State)
	}
	if wctx, _ := e.work(context.Background()); wctx != nil {
		t.Error("work started after stop_all")
	}
	call(t, s, "resume", nil, &d)
	if d.State != "idle" {
		t.Errorf("resume: state %q", d.State)
	}
	select {
	case <-e.poke:
	default:
		t.Error("resume didn't ask for a poll")
	}

	// Bad arguments are tool errors, and duckdb isn't run (e.duck is nil).
	for _, args := range []map[string]any{{"pipe": "nope"}, {"status": "ok"}} {
		if res := call(t, s, "load_history", args, nil); !res.IsError {
			t.Errorf("load_history %v: %+v", args, res)
		}
	}
}

// load_history filters by pipe and status, and keeps to limit. It needs the
// duckdb CLI; the history table is made by the setup of each run.
func TestMCPHistory(t *testing.T) {
	bin, err := exec.LookPath("duckdb")
	if err != nil {
		t.Skip("no duckdb CLI")
	}
	e := testEngine(t)
	e.cfg.Pipes = append(e.cfg.Pipes, &pipe{Name: "crime", Bucket: "home", Prefix: "uk/", Pattern: "*.csv", Table: "raw.crime"})
	e.hist = "h"
	e.duck = &duck{bin: bin, setup: []string{`CREATE TABLE h AS SELECT * FROM (VALUES
		('orders', 'home', 'raw/orders/a.csv', 'loaded', 10, NULL, TIMESTAMPTZ '2026-10-20 10:00:00+00', 'lake.raw.orders', 100),
		('orders', 'home', 'raw/orders/b.csv', 'failed', 0, 'bad row', TIMESTAMPTZ '2026-10-21 10:00:00+00', NULL, 50),
		('orders', 'home', 'raw/orders/c.csv', 'failed', 0, 'fixed since', TIMESTAMPTZ '2026-10-19 10:00:00+00', NULL, 60),
		('orders', 'home', 'raw/orders/c.csv', 'loaded', 5, NULL, TIMESTAMPTZ '2026-10-22 10:00:00+00', 'lake.raw.orders', 60),
		('crime', 'home', 'uk/x.csv', 'loaded', 7, NULL, TIMESTAMPTZ '2026-10-23 10:00:00+00', 'lake.raw.crime', 70)
	) AS t(pipe, bucket, key, status, rows, error, loaded_at, target, size)`,
		"ALTER TABLE h ADD COLUMN file_stats JSON"}}
	s, err := connect(t, e, "mcp-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var v historyView
	if res := call(t, s, "load_history", map[string]any{"pipe": "orders", "status": "loaded", "limit": 1}, &v); res.IsError {
		t.Fatalf("load_history: %+v", res.Content)
	}
	if len(v.History) != 1 || v.History[0].File != "s3://home/raw/orders/c.csv" || v.History[0].Rows != 5 {
		t.Errorf("history = %+v", v.History)
	}
	if len(v.Stats) != 2 || v.Stats[0].Pipe != "orders" || v.Stats[0].Files != 2 || v.Stats[0].Failed != 1 {
		t.Errorf("stats = %+v", v.Stats)
	}
	// The history keeps every failed attempt; the failed files are only
	// those whose latest attempt failed.
	if len(v.FailedFiles) != 1 || v.FailedFiles[0].File != "s3://home/raw/orders/b.csv" || v.FailedFiles[0].Error != "bad row" ||
		v.FailedFiles[0].Pipe != "orders" || v.FailedFiles[0].At == "" {
		t.Errorf("failed files = %+v", v.FailedFiles)
	}
	call(t, s, "load_history", map[string]any{"status": "failed"}, &v)
	if len(v.History) != 2 || v.History[0].Error != "bad row" || v.Limit != defaultHistory {
		t.Errorf("failed history = %+v", v)
	}

	// The page filters by status the same way.
	get := func(q string) (int, historyView) {
		rec := httptest.NewRecorder()
		e.apiHistory(rec, httptest.NewRequest("GET", "/api/history"+q, nil))
		var v historyView
		json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	if code, v := get("?pipe=orders&status=failed"); code != 200 || len(v.History) != 2 || len(v.FailedFiles) != 1 {
		t.Errorf("api/history failed: %d %+v", code, v)
	}
	if code, _ := get("?status=ok"); code != http.StatusBadRequest {
		t.Errorf("api/history status=ok: %d", code)
	}
}

func TestMCPAuth(t *testing.T) {
	e := testEngine(t)
	if _, err := connect(t, e, "wrong"); err == nil {
		t.Error("connected with a wrong token")
	}
	// The page's password doesn't open /mcp.
	srv := httptest.NewServer(e.handler("admin:pw", "mcp-token"))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{}`))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("basic auth on /mcp: %d", resp.StatusCode)
	}
}
