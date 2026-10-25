package main

// The page and the JSON API behind it, both behind a password (basic auth,
// ATLAS_AUTH). GET /healthz stays open.

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// rebuildEvery is how stale a graph may be before a page load builds it again.
const rebuildEvery = 10 * time.Second

// rereadRunsEvery is how stale what is running may be, so several open pages
// don't each ask relay.
const rereadRunsEvery = 3 * time.Second

type atlas struct {
	paths paths
	relay *relayClient
	ls    lsFunc
	links []link

	mu    sync.Mutex
	last  *graph
	built time.Time

	runMu   sync.Mutex
	running *runningView
}

// runningView is GET /api/running.
type runningView struct {
	Runs  []*run    `json:"runs"`
	Relay string    `json:"relay_error,omitempty"`
	Read  time.Time `json:"read"`
}

// link is where a service's own page is: a port and a path on the host the
// page was opened on. The Services tab lists them all, with whether each
// answers.
type link struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Path string `json:"path"`
}

// parseLinks reads "name=port[/path] ..." separated by spaces or commas.
func parseLinks(s string) ([]link, error) {
	var out []link
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' }) {
		name, rest, ok := strings.Cut(f, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("%q: want name=port[/path]", f)
		}
		port, path, _ := strings.Cut(rest, "/")
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%q: bad port %q", f, port)
		}
		out = append(out, link{name, n, "/" + path})
	}
	return out, nil
}

// graph reads the configs and relay again, at most every rebuildEvery.
func (a *atlas) graph(ctx context.Context) *graph {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last != nil && time.Since(a.built) < rebuildEvery {
		return a.last
	}
	rc := a.relay.fetch(ctx)
	a.last = build(readConfigs(a.paths), rc, a.ls, time.Now())
	a.built = time.Now()
	return a.last
}

// runs reads what is running from relay, at most every rereadRunsEvery. It
// reads no config files: runs are placed on the graph last built, if any.
func (a *atlas) runs(ctx context.Context) *runningView {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.running != nil && time.Since(a.running.Read) < rereadRunsEvery {
		return a.running
	}
	now := time.Now()
	v := &runningView{Runs: []*run{}, Read: now}
	if re, err := a.relay.fetchRuns(ctx, now); err != nil {
		v.Relay = err.Error()
	} else {
		v.Runs = runs(re, now)
		a.mu.Lock()
		g := a.last
		a.mu.Unlock()
		placeRuns(v.Runs, g)
	}
	a.running = v
	return v
}

func (a *atlas) routes(auth string) http.Handler {
	mux := http.NewServeMux()
	private := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, basicAuth(auth, h)) }

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	private("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Write(uiHTML)
	})
	private("GET /api/graph", func(w http.ResponseWriter, r *http.Request) {
		g := a.graph(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(struct {
			*graph
			Links []link `json:"links"`
		}{g, a.links})
	})
	private("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(statuses(r.Context(), a.links))
	})
	private("GET /api/running", func(w http.ResponseWriter, r *http.Request) {
		v := a.runs(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	})
	return mux
}

func basicAuth(auth string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(auth)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="atlas"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
