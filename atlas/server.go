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

type atlas struct {
	paths paths
	relay *relayClient
	ls    lsFunc
	links []link

	mu    sync.Mutex
	last  *graph
	built time.Time
}

// link is where a service's own page is: a port and a path on the host the
// page was opened on.
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
