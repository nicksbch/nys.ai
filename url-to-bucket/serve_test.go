package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveKey(t *testing.T) {
	for _, tc := range []struct{ url, key, want string }{
		{"https://x.org/a/file.iso", "", "file.iso"},
		{"https://x.org/a/file.iso", "isos/", "isos/file.iso"},
		{"https://x.org/a/file.iso", "isos/other.iso", "isos/other.iso"},
		{"https://x.org/", "", "download"},
	} {
		if got := resolveKey(tc.url, tc.key); got != tc.want {
			t.Errorf("resolveKey(%q, %q) = %q, want %q", tc.url, tc.key, got, tc.want)
		}
	}
}

func TestRequestJob(t *testing.T) {
	ev := requestEvent{ID: 7, Data: json.RawMessage(`{"url":"https://x.org/f.csv","bucket":"home","key":"in/"}`)}
	j, err := ev.job()
	if err != nil {
		t.Fatal(err)
	}
	want := job{URL: "https://x.org/f.csv", Bucket: "home", Key: "in/f.csv", RunID: "req-7", Cause: 7}
	if j != want {
		t.Errorf("job = %+v, want %+v", j, want)
	}

	ev.RunID = "abc"
	if j, _ := ev.job(); j.RunID != "abc" {
		t.Errorf("run ID = %q, want the request's", j.RunID)
	}

	for _, data := range []string{
		``,
		`"https://x.org/f.csv"`,
		`{"url":"https://x.org/f.csv"}`,
		`{"url":"file:///etc/passwd","bucket":"home"}`,
		`{"url":"not a url","bucket":"home"}`,
	} {
		if _, err := (requestEvent{ID: 1, Data: json.RawMessage(data)}).job(); err == nil {
			t.Errorf("data %s: expected an error", data)
		}
	}
}

// fakeRelay serves requests from a fixed list after the "after" ID, one at
// a time like the service asks for them, and records acks.
type fakeRelay struct {
	mu     sync.Mutex
	events []requestEvent
	acked  []uint64
}

func (f *fakeRelay) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/consumers/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cursor":0}`))
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		evs := []requestEvent{}
		for _, e := range f.events {
			if e.ID > after {
				evs = append(evs, e)
				break
			}
		}
		if len(evs) == 0 {
			time.Sleep(10 * time.Millisecond) // stands in for the long-poll
		}
		json.NewEncoder(w).Encode(map[string]any{"events": evs})
	})
	mux.HandleFunc("POST /v1/consumers/{name}/ack", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.acked = append(f.acked, req.ID)
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	})
	return mux
}

func (f *fakeRelay) acks() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.acked)
}

// waitFor polls cond for up to 2 s.
func waitFor(cond func() bool) bool {
	for i := 0; i < 400; i++ {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestConsumerDoesNotWaitForSlowRequest(t *testing.T) {
	f := &fakeRelay{events: []requestEvent{{ID: 3}, {ID: 5}, {ID: 7}}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := &consumer{url: srv.URL, name: "url-to-bucket", http: srv.Client()}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var running, peak atomic.Int32
	var mu sync.Mutex
	var order []uint64
	release := make(chan struct{})
	go func() {
		// Request 3 is slow: 5 and 7 must finish meanwhile, with nothing acked.
		if !waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 2 }) {
			t.Error("5 and 7 did not run while 3 was running")
		}
		if a := f.acks(); len(a) != 0 {
			t.Errorf("acked %v while 3 was still running", a)
		}
		close(release)
		if !waitFor(func() bool { return slices.Contains(f.acks(), 7) }) {
			t.Error("7 was never acked")
		}
		cancel()
	}()
	c.run(ctx, 2, func(ctx context.Context, ev requestEvent) {
		n := running.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if ev.ID == 3 {
			<-release
		} else {
			time.Sleep(20 * time.Millisecond)
		}
		running.Add(-1)
		mu.Lock()
		order = append(order, ev.ID)
		mu.Unlock()
	})

	if peak.Load() != 2 {
		t.Errorf("peak parallel requests = %d, want 2", peak.Load())
	}
	if !slices.Equal(order, []uint64{5, 7, 3}) {
		t.Errorf("finish order = %v, want [5 7 3]", order)
	}
	if a := f.acks(); !slices.Equal(a, []uint64{7}) {
		t.Errorf("acks = %v, want [7]", a)
	}
}

func TestConsumerDoesNotAckWhatWasCutShort(t *testing.T) {
	f := &fakeRelay{events: []requestEvent{{ID: 3}, {ID: 5}}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := &consumer{url: srv.URL, name: "url-to-bucket", http: srv.Client()}

	ctx, cancel := context.WithCancel(context.Background())
	c.run(ctx, 2, func(ctx context.Context, ev requestEvent) {
		if ev.ID == 5 {
			// Stop the service while 5 runs, once 3 is acked.
			waitFor(func() bool { return slices.Contains(f.acks(), 3) })
			cancel()
		}
	})
	if a := f.acks(); !slices.Equal(a, []uint64{3}) {
		t.Errorf("acks = %v, want [3]", a)
	}
}

func TestRuns(t *testing.T) {
	ev := func(id uint64, typ, runID, data string) historyEvent {
		var e historyEvent
		json.Unmarshal([]byte(fmt.Sprintf(`{"id":%d,"type":%q,"run_id":%q,"data":%s}`, id, typ, runID, data)), &e)
		return e
	}
	events := []historyEvent{
		ev(1, "requested", "a", `{"url":"https://x.org/a.iso","bucket":"home","key":"isos/"}`),
		ev(2, "started", "a", `{"url":"https://x.org/a.iso","bucket":"home","key":"isos/a.iso"}`),
		ev(3, "requested", "", `{"url":"https://x.org/b.iso","bucket":"home"}`),
		ev(4, "ok", "a", `{"url":"https://x.org/a.iso","bucket":"home","key":"isos/a.iso","bytes":10,"duration_ms":5}`),
		ev(5, "requested", "c", `{"url":"https://x.org/c.iso","bucket":"home"}`),
		ev(6, "started", "c", `{"url":"https://x.org/c.iso","bucket":"home","key":"c.iso"}`),
		ev(7, "started", "cli", `{"url":"https://x.org/d.iso","bucket":"home","key":"d.iso"}`),
	}
	got := runs(events, map[string]liveProgress{"c": {40, 100, 6e6}})

	type row struct {
		run, state string
		bytes      int64
		live       bool
	}
	var rows []row
	for _, d := range got {
		rows = append(rows, row{d.RunID, d.State, d.Bytes, d.Live})
	}
	want := []row{
		{"cli", "running", 0, false},
		{"c", "running", 40, true},
		{"req-3", "queued", 0, false},
		{"a", "ok", 10, false},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("runs = %+v, want %+v", rows, want)
	}
	if got[3].Key != "isos/a.iso" || got[2].Key != "b.iso" {
		t.Errorf("keys %q, %q: want them resolved", got[3].Key, got[2].Key)
	}
	if got[1].Total != 100 || got[1].Rate != 6e6 {
		t.Errorf("total %d rate %v: want the live ones", got[1].Total, got[1].Rate)
	}
}
