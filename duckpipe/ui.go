package main

// The web page served by "duckpipe run": the batch being loaded, each pipe's
// last poll and the files still to load, the load history, and "Stop all",
// which stops the load under way and pauses polling until "Resume". Pipes
// come from the config and can't be changed here. The same, as MCP tools, is
// at /mcp (mcp.go).

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// historyLimit is how many load history rows the page gets at most.
const historyLimit = 500

// serve runs the page on addr until ctx ends. auth is "user:pass" for basic
// auth, or empty for none. With mcpToken, the MCP tools are at /mcp behind it.
func (e *engine) serve(ctx context.Context, addr, auth, mcpToken string) {
	srv := &http.Server{Addr: addr, Handler: e.handler(auth, mcpToken), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("page on http://%s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (e *engine) handler(auth, mcpToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})
	mux.HandleFunc("GET /api/state", e.apiState)
	mux.HandleFunc("GET /api/history", e.apiHistory)
	mux.HandleFunc("POST /api/stop", e.apiStop)
	mux.HandleFunc("POST /api/resume", e.apiResume)
	if mcpToken != "" {
		mux.Handle("/mcp", bearerAuth(mcpToken, e.mcpHandler()))
	} else {
		log.Print("DUCKPIPE_MCP_TOKEN is not set: no MCP tools at /mcp")
	}
	return guard(auth, mux)
}

// guard adds optional basic auth, and requires a custom header on every
// state-changing request so other web pages can't forge them (a cross-origin
// request with a custom header needs a CORS preflight, which we never grant).
// /healthz is open, and /mcp has its own bearer token instead: it is for
// mcp-hub, not a browser.
func guard(auth string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		if r.URL.Path == "/mcp" {
			next.ServeHTTP(w, r)
			return
		}
		if auth != "" {
			user, pass, _ := r.BasicAuth()
			if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(auth)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="duckpipe"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Duckpipe") == "" {
			http.Error(w, "missing X-Duckpipe header", http.StatusForbidden)
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

// --- What "run" is doing ---------------------------------------------------

// work starts a poll (and maintenance) that "Stop all" can cancel. While
// stopped it returns a nil context. done must be called when the work ends.
func (e *engine) work(ctx context.Context) (wctx context.Context, done func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil, nil
	}
	wctx, cancel := context.WithCancelCause(ctx)
	e.cancel = cancel
	return wctx, func() {
		e.mu.Lock()
		e.cancel = nil
		e.mu.Unlock()
		cancel(nil)
	}
}

// stop cancels the load or maintenance under way, if any, and keeps "run"
// from polling until resume. The load's transaction is rolled back, and its
// files are loaded again after resume.
func (e *engine) stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	if e.cancel != nil {
		e.cancel(errStopped)
	}
}

func (e *engine) resume() {
	e.mu.Lock()
	e.stopped = false
	e.mu.Unlock()
	select {
	case e.poke <- struct{}{}:
	default: // a poll is already asked for
	}
}

func (e *engine) setLoading(p *pipe, batch []object, start time.Time) {
	var l *loading
	if p != nil {
		l = &loading{Pipe: p.Name, Table: e.targetName(p), Bucket: p.Bucket, Started: start}
		for _, o := range batch {
			l.Keys = append(l.Keys, o.Key)
			l.Bytes += o.Size
		}
	}
	e.mu.Lock()
	e.loading = l
	e.mu.Unlock()
}

func (e *engine) setMaintaining(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.maintaining = time.Time{}
	if on {
		e.maintaining = time.Now()
	}
}

// setWaiting records the files a pipe still has to load.
func (e *engine) setWaiting(name string, todo []object) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.pipes[name]
	s.Waiting, s.WaitingBytes = len(todo), 0
	for _, o := range todo {
		s.WaitingBytes += o.Size
	}
	e.pipes[name] = s
}

// setPolled records the end of a pipe's poll, and why it failed.
func (e *engine) setPolled(name string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.pipes[name]
	s.PolledAt, s.Error = time.Now(), ""
	if err != nil {
		s.Error = firstLine(err.Error())
	}
	e.pipes[name] = s
}

// --- API -------------------------------------------------------------------

// stateView is what the page polls every few seconds. It comes from memory,
// so it is cheap.
type stateView struct {
	// "loading", "maintenance", "idle" (between polls; also while listing),
	// "stopping" (stopped, the load not ended yet) or "stopped".
	State       string     `json:"state"`
	Poll        string     `json:"poll"`
	Loading     *loading   `json:"loading"`
	Maintaining *time.Time `json:"maintaining"`
	Pipes       []pipeView `json:"pipes"`
}

type pipeView struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Table  string `json:"table"`
	pipeState
}

func (e *engine) apiState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, e.stateView())
}

func (e *engine) stateView() stateView {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := stateView{State: "idle", Poll: e.cfg.Poll.Duration.String(), Loading: e.loading, Pipes: []pipeView{}}
	if !e.maintaining.IsZero() {
		t := e.maintaining
		v.Maintaining = &t
	}
	switch {
	case e.stopped && e.cancel != nil:
		v.State = "stopping"
	case e.stopped:
		v.State = "stopped"
	case e.loading != nil:
		v.State = "loading"
	case v.Maintaining != nil:
		v.State = "maintenance"
	}
	for _, p := range e.cfg.Pipes {
		v.Pipes = append(v.Pipes, pipeView{Name: p.Name, Source: "s3://" + p.Bucket + "/" + p.Prefix + p.Pattern,
			Table: e.targetName(p), pipeState: e.pipes[p.Name]})
	}
	return v
}

// historyView is read from the lake, by running duckdb, so the page asks for
// it less often.
type historyView struct {
	Stats []statView `json:"stats"`
	// The files whose latest attempt failed, every pipe's, newest first, at
	// most failedLimit: what is failing now. History also has earlier
	// attempts that failed and loaded later.
	FailedFiles []failView `json:"failed_files"`
	History     []histView `json:"history"` // newest first, at most Limit
	Limit       int        `json:"limit"`
}

type failView struct {
	At    string `json:"at"` // UTC, "2006-01-02 15:04:05"
	Pipe  string `json:"pipe"`
	File  string `json:"file"`
	Error string `json:"error"`
}

type statView struct {
	Pipe   string `json:"pipe"`
	Files  int64  `json:"files"`
	Rows   int64  `json:"rows"`
	Failed int64  `json:"failed"`
	Last   string `json:"last,omitempty"` // UTC, "2006-01-02 15:04:05"
}

type histView struct {
	At     string `json:"at"` // UTC, "2006-01-02 15:04:05"
	Pipe   string `json:"pipe"`
	File   string `json:"file"`
	Target string `json:"target"`
	Status string `json:"status"`
	Rows   int64  `json:"rows"`
	Size   int64  `json:"size"`
	Error  string `json:"error,omitempty"`
}

// apiHistory returns each pipe's totals, the failed files and the newest
// history rows, optionally for one pipe (?pipe=) and one status (?status=).
func (e *engine) apiHistory(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && status != "loaded" && status != "failed" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("status is loaded or failed, not %q", status))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	v, err := e.historyView(ctx, r.URL.Query().Get("pipe"), status, historyLimit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// historyView is each pipe's totals and the newest limit history rows, for
// one pipe and one status if they aren't empty.
func (e *engine) historyView(ctx context.Context, pipe, status string, limit int) (historyView, error) {
	stats, failed, err := e.stats(ctx)
	if err != nil {
		return historyView{}, err
	}
	rows, err := e.historyRows(ctx, pipe, status, limit)
	if err != nil {
		return historyView{}, err
	}
	v := historyView{Stats: []statView{}, FailedFiles: []failView{}, History: []histView{}, Limit: limit}
	for _, p := range e.cfg.Pipes {
		s := stats[p.Name]
		v.Stats = append(v.Stats, statView{Pipe: p.Name, Files: s.int("files"), Rows: s.int("rows"),
			Failed: s.int("failed"), Last: s.str("last")})
	}
	for _, f := range failed {
		v.FailedFiles = append(v.FailedFiles, failView{At: f.str("at"), Pipe: f.str("pipe"), File: f.str("file"),
			Error: f.str("error")})
	}
	for _, h := range rows {
		v.History = append(v.History, histView{At: h.str("at"), Pipe: h.str("pipe"), File: h.str("file"),
			Target: h.str("target"), Status: h.str("status"), Rows: h.int("rows"), Size: h.int("size"),
			Error: h.str("error")})
	}
	return v, nil
}

func (e *engine) apiStop(w http.ResponseWriter, r *http.Request) {
	log.Printf("page: stop all")
	e.stop()
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (e *engine) apiResume(w http.ResponseWriter, r *http.Request) {
	log.Printf("page: resume")
	e.resume()
	writeJSON(w, http.StatusOK, map[string]any{})
}
