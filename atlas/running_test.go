package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func runByID(rs []*run, id string) *run {
	for _, r := range rs {
		if r.RunID == id {
			return r
		}
	}
	return nil
}

func TestRuns(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	re := &runEvents{
		Starts: []event{
			{ID: 20, Time: at(time.Minute), Topic: "duckpipe.ingest", Type: "started", RunID: "d1", Data: map[string]any{"name": "crime_street: 3 files", "pipe": "crime_street"}},
			{ID: 19, Time: at(2 * time.Minute), Topic: "url-to-bucket.download", Type: "requested", RunID: "u1", Data: map[string]any{}},
			{ID: 18, Time: at(5 * time.Minute), Topic: "lakeshape.run", Type: "started", RunID: "l1", Data: map[string]any{"jobs": []any{"crime"}, "models": 4.0}},
			{ID: 17, Time: at(6 * time.Minute), Topic: "lakeshape.trigger", Type: "requested", RunID: "l1", Data: map[string]any{"job": "crime"}},
			{ID: 16, Time: at(10 * time.Minute), Topic: "preprocessor.unzip", Type: "started", RunID: "p1", Data: map[string]any{"name": "uk_crime: a.zip"}},
			{ID: 15, Time: at(20 * time.Minute), Topic: "foreman.oncall", Type: "started", RunID: "f1", Data: map[string]any{"name": "duckpipe failed"}},
			{ID: 14, Time: at(time.Hour), Topic: "preprocessor.unzip", Type: "started", RunID: "p0", Data: map[string]any{"name": "old.zip"}},
			{ID: 13, Time: at(7 * time.Hour), Topic: "duckpipe.ingest", Type: "started", RunID: "lost", Data: map[string]any{"name": "killed"}},
			{ID: 12, Time: at(8 * time.Hour), Topic: "lakeshape.trigger", Type: "requested", RunID: "l2", Data: map[string]any{"job": "nope"}},
			{ID: 11, Time: at(30 * time.Hour), Topic: "duckpipe.ingest", Type: "started", RunID: "ancient"},
		},
		Ends: []event{
			{ID: 30, Time: at(30 * time.Second), Topic: "preprocessor.unzip", Type: "failed", RunID: "p1", Data: map[string]any{"error": "bad zip\nmore"}},
			// mcp-hub's calls carry foreman's run ID; they don't end its run.
			{ID: 29, Time: at(time.Minute), Topic: "mcp-hub.call", Type: "ok", RunID: "f1"},
			{ID: 28, Time: at(50 * time.Minute), Topic: "preprocessor.unzip", Type: "ok", RunID: "p0"},
			// A trigger for a job that doesn't exist ends on lakeshape.run.
			{ID: 27, Time: at(8 * time.Hour), Topic: "lakeshape.run", Type: "failed", RunID: "l2"},
		},
		ByRun: map[string][]event{
			"l1": {
				{ID: 26, Time: at(time.Minute), Topic: "lakeshape.model", Type: "ok", RunID: "l1"},
				{ID: 25, Time: at(2 * time.Minute), Topic: "lakeshape.model", Type: "failed", RunID: "l1"},
				{ID: 18, Time: at(5 * time.Minute), Topic: "lakeshape.run", Type: "started", RunID: "l1", Data: map[string]any{"jobs": []any{"crime"}, "models": 4.0}},
			},
		},
	}
	rs := runs(re, now)
	for id, want := range map[string]string{"d1": "running", "u1": "waiting", "l1": "running", "p1": "ended", "f1": "running", "lost": "no_result"} {
		r := runByID(rs, id)
		if r == nil {
			t.Errorf("%s: missing", id)
			continue
		}
		if r.State != want {
			t.Errorf("%s: state %s, want %s", id, r.State, want)
		}
	}
	for _, id := range []string{"p0", "l2", "ancient"} {
		if runByID(rs, id) != nil {
			t.Errorf("%s: shown, want left out", id)
		}
	}
	if r := runByID(rs, "p1"); r != nil && (r.Result != "failed" || r.Error != "bad zip") {
		t.Errorf("p1: result %q error %q", r.Result, r.Error)
	}
	if r := runByID(rs, "l1"); r != nil && (r.Done != 2 || r.Total != 4 || r.Name != "jobs crime" || !r.Since.Equal(at(5*time.Minute))) {
		t.Errorf("l1: %d of %d, name %q, since %v", r.Done, r.Total, r.Name, r.Since)
	}

	got := strings.Join(unanswered(re.Starts, re.Ends, now), " ")
	if want := "d1 f1 l1 lost u1"; got != want {
		t.Errorf("unanswered: %q, want %q", got, want)
	}
}

func TestPlaceRuns(t *testing.T) {
	g := build(readConfigs(repoPaths("..")), &recent{}, stubLs, time.Now())
	rs := []*run{
		{RunID: "a", start: event{Topic: "duckpipe.ingest", Type: "started", Data: map[string]any{"pipe": "crime_street"}}},
		{RunID: "b", start: event{Topic: "lakeshape.run", Type: "started", Data: map[string]any{"jobs": []any{"crime"}}}},
	}
	placeRuns(rs, g)
	if got := strings.Join(rs[0].Nodes, " "); got != "pipe:crime_street" {
		t.Errorf("duckpipe run on %q", got)
	}
	if got := strings.Join(rs[1].Nodes, " "); got != "job:crime" {
		t.Errorf("lakeshape run on %q", got)
	}
}

// GET /api/running asks relay for starts, results, and each unanswered run.
func TestAPIRunning(t *testing.T) {
	now := time.Now()
	var asked []string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RawQuery)
		q := r.URL.Query()
		var evs []event
		switch {
		case q.Get("type") == "started,requested":
			evs = []event{{ID: 1, Time: now, Topic: "duckpipe.ingest", Type: "started", RunID: "r1", Data: map[string]any{"name": "loading"}}}
		case q.Get("run_id") == "r1":
			evs = []event{{ID: 1, Time: now, Topic: "duckpipe.ingest", Type: "started", RunID: "r1", Data: map[string]any{"name": "loading"}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"events": evs})
	}))
	defer relay.Close()
	a := &atlas{relay: &relayClient{url: relay.URL, client: relay.Client()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/running", nil)
	req.SetBasicAuth("me", "pw")
	a.routes("me:pw").ServeHTTP(rec, req)
	var v runningView
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Relay != "" || len(v.Runs) != 1 || v.Runs[0].State != "running" || v.Runs[0].Name != "loading" {
		t.Errorf("got %+v", v)
	}
	if len(asked) != 3 || !strings.Contains(asked[2], "run_id=r1") {
		t.Errorf("asked relay %q", asked)
	}
}
