package main

// The web page served by "url-to-bucket serve": a form that publishes a
// request to relay, and the recent downloads with live progress. History
// comes from relay's events; bytes and totals of the downloads running in
// this process come from their progress counters.

import (
	"cmp"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	relay "relay/client"
)

//go:embed ui.html
var uiHTML []byte

const (
	historyEvents = 500 // relay events read for the list
	shownRuns     = 100 // downloads shown
)

type ui struct {
	bus    *relay.Client
	relay  *consumer // for reading events
	s3     *s3.Client
	mu     sync.Mutex
	active map[string]*progress // run ID -> downloads running here
}

func (u *ui) track(runID string, p *progress) func() {
	u.mu.Lock()
	u.active[runID] = p
	u.mu.Unlock()
	return func() {
		u.mu.Lock()
		delete(u.active, runID)
		u.mu.Unlock()
	}
}

// serve runs the page on addr until ctx ends. auth is "user:pass" for basic
// auth, or empty for none.
func (u *ui) serve(ctx context.Context, addr, auth string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})
	mux.HandleFunc("GET /api/buckets", u.buckets)
	mux.HandleFunc("GET /api/downloads", u.downloads)
	mux.HandleFunc("POST /api/downloads", u.request)
	srv := &http.Server{Addr: addr, Handler: guard(auth, mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("ui on http://%s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// guard adds optional basic auth, and requires a custom header on every
// state-changing request so other web pages can't forge them (a cross-origin
// request with a custom header needs a CORS preflight, which we never grant).
// /healthz is open.
func guard(auth string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		if auth != "" {
			user, pass, _ := r.BasicAuth()
			if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(auth)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="url-to-bucket"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Url-To-Bucket") == "" {
			http.Error(w, "missing X-Url-To-Bucket header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// buckets lists the buckets the service's key can see.
func (u *ui) buckets(w http.ResponseWriter, r *http.Request) {
	out, err := u.s3.ListBuckets(r.Context(), &s3.ListBucketsInput{})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	names := []string{}
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
	}
	writeJSON(w, http.StatusOK, names)
}

// request publishes a "requested" event, the same one relay pub would.
func (u *ui) request(w http.ResponseWriter, r *http.Request) {
	var data json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&data); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("body must be JSON: %w", err))
		return
	}
	if _, err := (requestEvent{Data: data}).job(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	runID := newRunID()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	ev, err := u.bus.Publish(ctx, relay.Event{Topic: topic, Type: "requested", RunID: runID, Data: data})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": runID, "id": ev.ID})
}

// historyEvent is the part of a relay event the list uses.
type historyEvent struct {
	ID    uint64    `json:"id"`
	Time  time.Time `json:"time"`
	Type  string    `json:"type"`
	RunID string    `json:"run_id"`
	Data  struct {
		URL        string `json:"url"`
		Bucket     string `json:"bucket"`
		Key        string `json:"key"`
		Bytes      int64  `json:"bytes"`
		DurationMS int64  `json:"duration_ms"`
		Error      string `json:"error"`
	} `json:"data"`
}

// download is one run as the page shows it.
type download struct {
	RunID      string    `json:"run_id"`
	State      string    `json:"state"` // queued, running, ok, failed
	URL        string    `json:"url"`
	Bucket     string    `json:"bucket"`
	Key        string    `json:"key"`
	Error      string    `json:"error,omitempty"`
	Bytes      int64     `json:"bytes"`
	Total      int64     `json:"total,omitempty"` // 0 if unknown
	Live       bool      `json:"live"`            // bytes are counted by this process right now
	Rate       float64   `json:"rate,omitempty"`  // bytes/s over the last few seconds, for live ones
	Time       time.Time `json:"time"`            // of the run's first event
	DurationMS int64     `json:"duration_ms,omitempty"`
	firstID    uint64
}

func (u *ui) downloads(w http.ResponseWriter, r *http.Request) {
	var resp struct {
		Events []historyEvent `json:"events"`
	}
	path := fmt.Sprintf("/v1/events?topic=%s&last=%d", topic, historyEvents)
	if err := u.relay.do(r.Context(), http.MethodGet, path, nil, &resp); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("reading relay: %w", err))
		return
	}
	u.mu.Lock()
	live := map[string]liveProgress{}
	for id, p := range u.active {
		live[id] = liveProgress{p.done.Load(), p.total.Load(), p.speed()}
	}
	u.mu.Unlock()
	writeJSON(w, http.StatusOK, runs(resp.Events, live))
}

// liveProgress is a download running in this process.
type liveProgress struct {
	done, total int64
	rate        float64
}

// runs folds events into one download per run, newest first. live holds the
// downloads in progress here, by run ID.
func runs(events []historyEvent, live map[string]liveProgress) []download {
	slices.SortFunc(events, func(a, b historyEvent) int { return cmp.Compare(a.ID, b.ID) })
	byRun := map[string]*download{}
	for _, e := range events {
		runID := e.RunID
		if runID == "" {
			runID = fmt.Sprintf("req-%d", e.ID) // what the service uses for it
		}
		d := byRun[runID]
		if d == nil {
			d = &download{RunID: runID, State: "queued", Time: e.Time, firstID: e.ID}
			byRun[runID] = d
		}
		if e.Data.URL != "" {
			d.URL, d.Bucket = e.Data.URL, e.Data.Bucket
			// A request may leave the key out or give a folder; from
			// "started" on, the events carry the resolved key.
			d.Key = resolveKey(e.Data.URL, e.Data.Key)
		}
		switch e.Type {
		case "started":
			d.State, d.Error = "running", ""
		case "ok", "failed":
			d.State, d.Bytes, d.DurationMS, d.Error = e.Type, e.Data.Bytes, e.Data.DurationMS, e.Data.Error
		}
	}
	out := make([]download, 0, len(byRun))
	for _, d := range byRun {
		if p, ok := live[d.RunID]; ok {
			d.State, d.Bytes, d.Total, d.Rate, d.Live = "running", p.done, p.total, p.rate, true
		}
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b download) int { return cmp.Compare(b.firstID, a.firstID) })
	if len(out) > shownRuns {
		out = out[:shownRuns]
	}
	return out
}
