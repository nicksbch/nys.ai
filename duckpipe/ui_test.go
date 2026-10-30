package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testEngine(t *testing.T) *engine {
	t.Helper()
	cfg := &config{Pipes: []*pipe{{Name: "orders", Bucket: "home", Prefix: "raw/orders/", Pattern: "*.csv", Table: "raw.orders", Options: "header = true"}}}
	cfg.Lake.Name = "lake"
	cfg.Lake.Catalog = "ducklake:sqlite:catalog.sqlite"
	if err := cfg.check(); err != nil {
		t.Fatal(err)
	}
	return &engine{cfg: cfg, lake: qi("lake"), down: map[string]bool{}, pipes: map[string]pipeState{}, poke: make(chan struct{}, 1)}
}

func getState(t *testing.T, e *engine) stateView {
	t.Helper()
	rec := httptest.NewRecorder()
	e.apiState(rec, httptest.NewRequest("GET", "/api/state", nil))
	var v stateView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Stop cancels the work under way with errStopped and keeps new work from
// starting; resume allows it again and asks for a poll.
func TestStopResume(t *testing.T) {
	e := testEngine(t)
	wctx, done := e.work(context.Background())
	if wctx == nil {
		t.Fatal("work refused before any stop")
	}
	e.stop()
	if !errors.Is(context.Cause(wctx), errStopped) {
		t.Errorf("cause = %v, want errStopped", context.Cause(wctx))
	}
	if s := getState(t, e).State; s != "stopping" {
		t.Errorf("state = %q, want stopping", s)
	}
	done()
	if s := getState(t, e).State; s != "stopped" {
		t.Errorf("state = %q, want stopped", s)
	}
	if wctx, _ := e.work(context.Background()); wctx != nil {
		t.Error("work started while stopped")
	}

	e.resume()
	select {
	case <-e.poke:
	default:
		t.Error("resume didn't ask for a poll")
	}
	wctx, done = e.work(context.Background())
	if wctx == nil {
		t.Fatal("work refused after resume")
	}
	done()
	if s := getState(t, e).State; s != "idle" {
		t.Errorf("state = %q, want idle", s)
	}
}

// A load stopped from the page kills its duckdb process right away, reports
// errStopped, and doesn't mark the files failed.
func TestStopKillsLoad(t *testing.T) {
	e := testEngine(t)
	bin := filepath.Join(t.TempDir(), "duckdb")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.duck = &duck{bin: bin}
	p := e.cfg.Pipes[0]
	off := false
	p.Stats = &off // no S3 to count the files from
	batch := []object{{Key: "raw/orders/a.csv", ETag: "x", Size: 10}, {Key: "raw/orders/b.csv", ETag: "y", Size: 5}}

	wctx, done := e.work(context.Background())
	defer done()
	errc := make(chan error, 1)
	go func() { errc <- e.loadBatch(wctx, p, batch, nil) }()

	deadline := time.Now().Add(5 * time.Second)
	for getState(t, e).Loading == nil {
		if time.Now().After(deadline) {
			t.Fatal("the load never showed as loading")
		}
		time.Sleep(10 * time.Millisecond)
	}
	l := getState(t, e).Loading
	if l.Pipe != "orders" || l.Table != "lake.raw.orders" || len(l.Keys) != 2 || l.Bytes != 15 {
		t.Errorf("loading = %+v", l)
	}

	start := time.Now()
	e.stop()
	select {
	case err := <-errc:
		if !errors.Is(err, errStopped) {
			t.Errorf("loadBatch = %v, want errStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the load didn't stop")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("stopping took %s", d)
	}
	if v := getState(t, e); v.Loading != nil {
		t.Errorf("still loading after stop: %+v", v.Loading)
	}
}

func TestGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := guard("admin:pw", ok)
	for _, c := range []struct {
		method, path string
		auth, header bool
		want         int
	}{
		{"GET", "/healthz", false, false, 200},
		{"GET", "/api/state", false, false, 401},
		{"GET", "/api/state", true, false, 200},
		{"POST", "/api/stop", true, false, 403},
		{"POST", "/api/stop", true, true, 200},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.auth {
			r.SetBasicAuth("admin", "pw")
		}
		if c.header {
			r.Header.Set("X-Duckpipe", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s %s auth=%v header=%v: %d, want %d", c.method, c.path, c.auth, c.header, rec.Code, c.want)
		}
	}
}
