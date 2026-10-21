package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	relay "relay/client"
)

func init() {
	retryFirst, retryMax, pollWait = 20*time.Millisecond, 50*time.Millisecond, "100ms"
}

// fakeRelay holds events for consumers and records what is published.
type fakeRelay struct {
	srv       *httptest.Server
	mu        sync.Mutex
	events    []relayEvent
	cursor    map[string]uint64
	filters   map[string]string
	published []relay.Event
}

func newFakeRelay(t *testing.T) *fakeRelay {
	f := &fakeRelay{cursor: map[string]uint64{}, filters: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case r.Method == "POST" && path == "events":
			var e relay.Event
			json.NewDecoder(r.Body).Decode(&e)
			f.published = append(f.published, e)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(e)
		case r.Method == "PUT" && strings.HasPrefix(path, "consumers/"):
			var filt map[string]string
			json.NewDecoder(r.Body).Decode(&filt)
			f.filters[strings.TrimPrefix(path, "consumers/")] = filt["topic"] + " " + filt["type"]
		case r.Method == "GET" && strings.HasSuffix(path, "/next"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "consumers/"), "/next")
			var out []relayEvent
			for _, e := range f.events {
				if e.ID > f.cursor[name] {
					out = append(out, e)
					break
				}
			}
			if out == nil {
				f.mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				f.mu.Lock()
			}
			json.NewEncoder(w).Encode(map[string]any{"events": out})
		case r.Method == "POST" && strings.HasSuffix(path, "/ack"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "consumers/"), "/ack")
			var ack map[string]uint64
			json.NewDecoder(r.Body).Decode(&ack)
			f.cursor[name] = ack["id"]
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) add(e relayEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeRelay) acked(name string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor[name]
}

func (f *fakeRelay) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.published {
		out = append(out, e.Type)
	}
	return out
}

func duckEvent(id uint64, pipe string) relayEvent {
	return relayEvent{ID: id, Time: time.Now(), Topic: "duckpipe.ingest", Type: "failed",
		Data: map[string]any{"pipe": pipe, "name": pipe + ": orders.csv -> lake.sales." + pipe, "error": "N/A is not a BIGINT"}}
}

type harness struct {
	e     *engine
	st    *store
	relay *fakeRelay
	model *fakeModel
	dir   string
	ctx   context.Context // cancelled when the test ends
}

func newTestEnv(t *testing.T, model *fakeModel, every time.Duration) *harness {
	t.Helper()
	hub := newFakeHub(t)
	rel := newFakeRelay(t)
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := openAIConf(model)
	other := openAIConf(model)
	defs := &Defs{
		Models: map[string]*ModelConf{"local": m, "other": other},
		Agents: map[string]*Agent{"oncall": {Name: "oncall", Profile: "oncall", Model: "local", MaxSteps: 5, Timeout: 10 * time.Second, Prompt: "On call."}},
		Types: map[string]*TaskType{"duckpipe-failure": {Name: "duckpipe-failure", Description: "Explain a failed ingestion",
			OnTopic: "duckpipe.*", OnTypes: []string{"failed"}, Every: every, Agent: "oncall",
			GroupBy: []string{"topic", "data.pipe"}, Output: []string{"explanation", "fix"}, Prompt: "Explain."}},
	}
	e := newEngine(defs, st, &relay.Client{URL: rel.srv.URL, HTTP: &http.Client{}}, hub.srv.URL, "hub-token", 2)
	ctx, cancel := context.WithCancel(context.Background())
	env := &harness{e: e, st: st, relay: rel, model: model, dir: dir, ctx: ctx}
	t.Cleanup(func() { cancel(); e.wait() })
	return env
}

// eventually waits for cond, failing the test after a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (env *harness) tasks() []*Task { return env.st.find(func(*Task) bool { return true }) }

func TestEventCreatesTaskAndRuns(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	env := newTestEnv(t, model, 0)
	env.relay.add(duckEvent(7, "orders"))
	env.e.start(env.ctx)

	eventually(t, "task answered", func() bool { ts := env.tasks(); return len(ts) == 1 && ts[0].Status == statusAnswered })
	eventually(t, "event acked", func() bool { return env.relay.acked("foreman-duckpipe-failure") == 7 })
	task := env.tasks()[0]
	if task.Origin != "event" || task.EventID != 7 || task.Key != "duckpipe-failure|duckpipe.ingest|orders" ||
		task.Title != "Explain a failed ingestion: orders: orders.csv -> lake.sales.orders" || task.Result["fix"] != "clean the file" {
		t.Errorf("task = %+v", task)
	}
	if !strings.HasPrefix(task.Input, "Event:\n{") || !strings.Contains(task.Input, `"error": "N/A is not a BIGINT"`) {
		t.Errorf("input = %q", task.Input)
	}
	if env.relay.filters["foreman-duckpipe-failure"] != "duckpipe.* failed" {
		t.Errorf("consumer filter = %q", env.relay.filters["foreman-duckpipe-failure"])
	}
	eventually(t, "relay events", func() bool { return strings.Join(env.relay.types(), ",") == "created,started,ok" })
	env.relay.mu.Lock()
	ok := env.relay.published[2]
	env.relay.mu.Unlock()
	data := ok.Data.(map[string]any)
	if ok.Topic != "foreman.duckpipe-failure" || ok.RunID == "" || data["task_id"] != task.ID ||
		data["result"].(map[string]any)["explanation"] != "N/A in quantity" {
		t.Errorf("ok event = %+v", ok)
	}

	// The transcript was stored.
	recs, err := env.st.readTranscript(task.Runs[0])
	if err != nil || len(recs) < 3 {
		t.Fatalf("transcript: %d records, %v", len(recs), err)
	}
}

func TestGrouping(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	env := newTestEnv(t, model, 0)
	env.relay.add(duckEvent(1, "orders"))
	env.relay.add(duckEvent(2, "orders"))
	env.relay.add(duckEvent(3, "customers"))
	env.e.start(env.ctx)
	eventually(t, "all acked", func() bool { return env.relay.acked("foreman-duckpipe-failure") == 3 })
	eventually(t, "both answered", func() bool {
		ts := env.tasks()
		return len(ts) == 2 && ts[0].Status == statusAnswered && ts[1].Status == statusAnswered
	})
	var orders *Task
	for _, tk := range env.tasks() {
		if strings.HasSuffix(tk.Key, "|orders") {
			orders = tk
		}
	}
	if orders.Seen != 2 || len(orders.Events) != 2 || len(orders.Runs) != 1 {
		t.Errorf("orders task = seen %d, events %v, %d runs", orders.Seen, orders.Events, len(orders.Runs))
	}

	// Once resolved, the next failure of the pipe is a new task.
	if err := env.e.setStatus(orders.ID, statusResolved); err != nil {
		t.Fatal(err)
	}
	env.relay.add(duckEvent(4, "orders"))
	eventually(t, "a third task", func() bool { return len(env.tasks()) == 3 })
}

func TestRedeliveredEventIsNotATaskTwice(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	env := newTestEnv(t, model, 0)
	env.e.defs.Types["duckpipe-failure"].GroupBy = nil // no grouping: only the event id prevents a duplicate
	env.relay.add(duckEvent(5, "orders"))
	env.e.start(env.ctx)
	eventually(t, "acked", func() bool { return env.relay.acked("foreman-duckpipe-failure") == 5 })

	// relay delivers it again, as after a crash before the ack.
	env.relay.mu.Lock()
	env.relay.cursor["foreman-duckpipe-failure"] = 0
	env.relay.mu.Unlock()
	eventually(t, "acked again", func() bool { return env.relay.acked("foreman-duckpipe-failure") == 5 })
	if n := len(env.tasks()); n != 1 {
		t.Errorf("%d tasks, want 1", n)
	}
}

func TestUnavailableModelIsRetried(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	model.status = 503
	env := newTestEnv(t, model, 0)
	env.relay.add(duckEvent(9, "orders"))
	env.e.start(env.ctx)

	eventually(t, "waiting", func() bool { ts := env.tasks(); return len(ts) == 1 && ts[0].Waiting != "" })
	if env.relay.acked("foreman-duckpipe-failure") != 0 {
		t.Error("the event was acked while the run couldn't take place")
	}
	if tk := env.tasks()[0]; tk.Status != statusCreated || tk.Runs[0].Status != "unavailable" {
		t.Errorf("task = %s, run %s", tk.Status, tk.Runs[0].Status)
	}

	model.mu.Lock()
	model.status = 0
	model.mu.Unlock()
	eventually(t, "answered", func() bool { return env.tasks()[0].Status == statusAnswered })
	eventually(t, "acked", func() bool { return env.relay.acked("foreman-duckpipe-failure") == 9 })
	for _, typ := range env.relay.types() {
		if typ == "failed" {
			t.Error(`an unavailable model was published as "failed"`)
		}
	}
	if tk := env.tasks()[0]; tk.Waiting != "" {
		t.Errorf("still waiting: %q", tk.Waiting)
	}
}

func TestRestartResumesInterruptedRun(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	env := newTestEnv(t, model, 0)
	tt := env.e.defs.Types["duckpipe-failure"]
	tt.OnTopic = "" // no relay trigger in this test
	tk, err := env.e.create(tt, "user", "x", "", "Look at this.", 0)
	if err != nil {
		t.Fatal(err)
	}
	// As if foreman had died mid-run.
	env.st.write(logRecord{Op: "run_started", Task: tk.ID, Run: &RunInfo{ID: "old", Agent: "oncall", Model: "local", Started: time.Now(), Status: "running"}})

	st2, err := openStore(env.dir) // what a restart reads
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.get(tk.ID); got.Status != statusRunning {
		t.Fatalf("after reopening: %s", got.Status)
	}
	env.e.store = st2
	env.e.start(env.ctx)
	eventually(t, "answered", func() bool { return st2.get(tk.ID).Status == statusAnswered })
	got := st2.get(tk.ID)
	if len(got.Runs) != 2 || got.Runs[0].Status != "interrupted" || got.Runs[1].Status != "ok" {
		t.Errorf("runs = %+v %+v", got.Runs[0], got.Runs[len(got.Runs)-1])
	}
}

func TestSchedule(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	env := newTestEnv(t, model, 50*time.Millisecond)
	env.e.defs.Types["duckpipe-failure"].OnTopic = ""
	env.e.start(env.ctx)
	eventually(t, "two scheduled tasks answered", func() bool {
		ts := env.tasks()
		return len(ts) >= 2 && ts[len(ts)-1].Status == statusAnswered && ts[len(ts)-2].Status == statusAnswered
	})
	ts := env.tasks()
	first, second := ts[len(ts)-1], ts[len(ts)-2]
	if first.Origin != "schedule" || !strings.Contains(first.Input, "This is the first scheduled task") ||
		!strings.Contains(second.Input, "Previous scheduled task: answered") {
		t.Errorf("inputs = %q / %q", first.Input, second.Input)
	}
}

func TestYourActions(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	env := newTestEnv(t, model, 0)
	env.e.defs.Types["duckpipe-failure"].OnTopic = ""
	ctx := env.ctx
	if _, err := env.e.createUserTask(ctx, "nope", "x"); err == nil {
		t.Error("unknown task type accepted")
	}
	tk, err := env.e.createUserTask(ctx, "duckpipe-failure", "Why did orders fail last night?\nMore detail.")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Title != "Explain a failed ingestion: Why did orders fail last night?" || tk.Origin != "user" {
		t.Errorf("task = %+v", tk)
	}
	eventually(t, "answered", func() bool { return env.st.get(tk.ID).Status == statusAnswered })

	// Run again with another model.
	if err := env.e.rerun(ctx, tk.ID, "", "nope"); err == nil {
		t.Error("unknown model accepted")
	}
	if err := env.e.rerun(ctx, tk.ID, "", "other"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second run", func() bool {
		got := env.st.get(tk.ID)
		return len(got.Runs) == 2 && got.Status == statusAnswered
	})
	if got := env.st.get(tk.ID); got.Runs[1].Model != "other" {
		t.Errorf("rerun model = %q", got.Runs[1].Model)
	}

	if err := env.e.setVerdict(tk.ID, "meh", ""); err == nil {
		t.Error("bad verdict accepted")
	}
	if err := env.e.setVerdict(tk.ID, "good", "spot on"); err != nil {
		t.Fatal(err)
	}
	if err := env.e.setStatus(tk.ID, "done"); err == nil {
		t.Error("bad status accepted")
	}
	if err := env.e.setStatus(tk.ID, statusDismissed); err != nil {
		t.Fatal(err)
	}
	if err := env.e.setStatus(tk.ID, statusResolved); err == nil {
		t.Error("a closed task was closed again")
	}
	if err := env.e.rerun(ctx, tk.ID, "", ""); err == nil {
		t.Error("a closed task was run again")
	}
	got := env.st.get(tk.ID)
	if got.Verdict != "good" || got.Note != "spot on" || got.Status != statusDismissed {
		t.Errorf("task = %+v", got)
	}
	eventually(t, "dismissed published", func() bool { return strings.HasSuffix(strings.Join(env.relay.types(), ","), ",dismissed") })
}

func TestOneRunPerAgent(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	model.delay = 100 * time.Millisecond
	var inFlight, most atomic.Int32
	inner := model.srv.Config.Handler
	model.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		defer inFlight.Add(-1)
		inner.ServeHTTP(w, r)
	})
	env := newTestEnv(t, model, 0)
	env.e.defs.Types["duckpipe-failure"].OnTopic = ""
	for i := 0; i < 3; i++ {
		env.e.createUserTask(env.ctx, "duckpipe-failure", "task")
	}
	eventually(t, "all answered", func() bool {
		ts := env.tasks()
		for _, tk := range ts {
			if tk.Status != statusAnswered {
				return false
			}
		}
		return len(ts) == 3
	})
	if most.Load() != 1 {
		t.Errorf("%d model requests at once, want 1 (one run per agent)", most.Load())
	}
}

func TestStoreReopenAndExpire(t *testing.T) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	env := newTestEnv(t, model, 0)
	env.e.defs.Types["duckpipe-failure"].OnTopic = ""
	ctx := env.ctx
	keep, _ := env.e.createUserTask(ctx, "duckpipe-failure", "keep me")
	drop, _ := env.e.createUserTask(ctx, "duckpipe-failure", "drop me")
	eventually(t, "answered", func() bool {
		return env.st.get(keep.ID).Status == statusAnswered && env.st.get(drop.ID).Status == statusAnswered
	})
	env.e.setStatus(drop.ID, statusResolved)
	dropRun := env.st.runPath(env.st.get(drop.ID).Runs[0])

	n, err := env.st.expire(time.Now().Add(time.Hour)) // everything closed is "old"
	if err != nil || n != 1 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	if _, err := os.Stat(dropRun); !os.IsNotExist(err) {
		t.Error("the dropped task's transcript is still there")
	}
	// The rewritten log reads back the same, and keeps taking writes.
	env.e.setVerdict(keep.ID, "bad", "too vague")
	st2, err := openStore(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	got := st2.get(keep.ID)
	if st2.get(drop.ID) != nil || got == nil || got.Status != statusAnswered || got.Verdict != "bad" || got.Result["fix"] != "clean the file" {
		t.Errorf("after reopening: %+v", got)
	}
	if _, err := st2.readTranscript(got.Runs[0]); err != nil {
		t.Errorf("kept transcript: %v", err)
	}
}
