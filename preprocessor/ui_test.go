package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func testEngine(t *testing.T) *engine {
	t.Helper()
	j := &job{Name: "uk_crime", Bucket: "home", Prefix: "uk/crime/", To: "uk/crime/extracted/"}
	cfg := &config{Jobs: []*job{j}}
	if err := cfg.check(); err != nil {
		t.Fatal(err)
	}
	return &engine{
		cfg:    cfg,
		state:  &store{file: filepath.Join(t.TempDir(), "state.json")},
		poke:   make(chan struct{}, 1),
		listed: map[string]listing{},
	}
}

var t0 = time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)

func rec(key, etag, status string, at int) record {
	return record{Job: "uk_crime", Bucket: "home", Key: key, ETag: etag, Status: status, At: t0.Add(time.Duration(at) * time.Minute)}
}

func TestView(t *testing.T) {
	e := testEngine(t)
	rs := []record{
		rec("uk/crime/2026-07.zip", "a", "failed", 1), // archive gone since
		rec("uk/crime/2026-08.zip", "b", "ok", 2),
		rec("uk/crime/2026-09.zip", "c", "ok", 3), // the archive has a new ETag now
	}
	listed := map[string]listing{"uk_crime": {At: t0, Archives: []object{
		{Key: "uk/crime/2026-08.zip", ETag: "b"},
		{Key: "uk/crime/2026-09.zip", ETag: "d"},
		{Key: "uk/crime/2026-10.zip", ETag: "e"},
	}}}
	busy := &unzipping{Job: "uk_crime", Key: "uk/crime/2026-09.zip"}
	v := view(e.cfg, listed, busy, rs)

	got := map[string]string{}
	for _, a := range v.Jobs[0].Archives {
		got[a.Key] = a.Status
	}
	want := map[string]string{"uk/crime/2026-08.zip": "ok", "uk/crime/2026-09.zip": "unzipping", "uk/crime/2026-10.zip": "pending"}
	for k, s := range want {
		if got[k] != s {
			t.Errorf("%s: status %q, want %q", k, got[k], s)
		}
	}
	if len(v.History) != 3 || v.History[0].Key != "uk/crime/2026-09.zip" {
		t.Fatalf("history not newest first: %+v", v.History)
	}
	for _, h := range v.History {
		if h.Current != (h.Key == "uk/crime/2026-08.zip") {
			t.Errorf("%s: current = %v", h.Key, h.Current)
		}
	}
}

func TestRedo(t *testing.T) {
	e := testEngine(t)
	for _, r := range []record{rec("uk/crime/2026-08.zip", "b", "ok", 1), rec("uk/crime/2026-09.zip", "c", "ok", 2)} {
		if err := e.state.add(r); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(guard("", http.HandlerFunc(e.apiRedo)))
	defer srv.Close()
	if code := post(t, srv.URL, map[string]string{"job": "uk_crime", "key": "uk/crime/2026-08.zip"}); code != http.StatusOK {
		t.Fatalf("redo: %d", code)
	}
	if code := post(t, srv.URL, map[string]string{"job": "uk_crime", "key": "uk/other/x.zip"}); code != http.StatusBadRequest {
		t.Errorf("redo of a key outside the job: %d", code)
	}
	rs, _ := e.state.load()
	done := doneIDs(rs)
	if done[rs[0].id()] || !done[rs[1].id()] || !rs[0].Redone {
		t.Errorf("after redo: %+v", rs)
	}
	select {
	case <-e.poke:
	default:
		t.Error("redo didn't ask for a poll")
	}
}

func TestRemove(t *testing.T) {
	e := testEngine(t)
	gone := rec("uk/crime/2026-07.zip", "a", "failed", 1)
	cur := rec("uk/crime/2026-08.zip", "b", "ok", 2)
	for _, r := range []record{gone, cur} {
		if err := e.state.add(r); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(guard("", http.HandlerFunc(e.apiRemove)))
	defer srv.Close()

	if code := post(t, srv.URL, gone); code != http.StatusConflict {
		t.Errorf("remove before the first poll: %d", code)
	}
	e.listed["uk_crime"] = listing{At: t0, Archives: []object{{Key: cur.Key, ETag: cur.ETag}}}
	if code := post(t, srv.URL, cur); code != http.StatusConflict {
		t.Errorf("remove of the current record: %d", code)
	}
	if code := post(t, srv.URL, gone); code != http.StatusOK {
		t.Errorf("remove: %d", code)
	}
	if code := post(t, srv.URL, gone); code != http.StatusNotFound {
		t.Errorf("remove twice: %d", code)
	}
	rs, _ := e.state.load()
	if len(rs) != 1 || rs[0].Key != cur.Key {
		t.Errorf("left: %+v", rs)
	}
}

func TestGuardHeader(t *testing.T) {
	srv := httptest.NewServer(guard("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	defer srv.Close()
	r, err := http.Post(srv.URL, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("POST without X-Preprocessor: %d", r.StatusCode)
	}
}

func post(t *testing.T, url string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("X-Preprocessor", "1")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return r.StatusCode
}
