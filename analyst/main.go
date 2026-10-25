// analyst: ask questions about the lake in a chat, answered by a model
// that can only query the semantic layer, through one mcp-hub profile. No
// SQL and no other tools. Every query's rows are shown as a table and a
// chart. A conversation keeps its history, so follow-ups work, and its
// queries can be saved as a report that runs again on the latest data,
// without the model.
//
// Usage:
//
//	analyst serve [-listen 127.0.0.1:8098] [-config config] [-hub http://127.0.0.1:8094] [-data data]
//	analyst check [-config config] [-hub http://127.0.0.1:8094]
//
// Secrets come from the environment: ANALYST_AUTH (the page's login, as
// user:pass), MCP_HUB_TOKEN, and the keys models.json names.
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"chatapi"
)

//go:embed index.html
var indexHTML []byte

type app struct {
	cfg    *chatapi.Config
	sem    *semantic
	store  *store
	limits turnLimits
	logf   func(string, ...any)

	mu   sync.Mutex
	busy map[string]bool // conversations with a turn running
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "serve" && os.Args[1] != "check") {
		fmt.Fprintln(os.Stderr, "usage: analyst serve|check [flags]   (analyst serve -h for the flags)")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8098", "address to serve the page on")
	configDir := fs.String("config", "config", "directory with models.json and prompt.md")
	hub := fs.String("hub", "http://127.0.0.1:8094", "mcp-hub")
	profile := fs.String("profile", "analyst", "mcp-hub profile: the only tools the model gets")
	server := fs.String("server", "semantic", "the semantic layer's name in mcp-hub; only its tools are used")
	dataDir := fs.String("data", "data", "where conversations and reports are kept")
	maxSteps := fs.Int("max-steps", 12, "model calls per question")
	timeout := fs.Duration("timeout", 10*time.Minute, "longest a question may take")
	fs.Parse(os.Args[2:])

	cfg, err := chatapi.LoadConfig(*configDir, os.Getenv, "/etc/analyst.env")
	if err != nil {
		fmt.Fprintln(os.Stderr, "analyst:", err)
		os.Exit(2)
	}
	sem := &semantic{url: strings.TrimRight(*hub, "/") + "/mcp/" + *profile, token: os.Getenv("MCP_HUB_TOKEN"), prefix: *server + "__"}

	if cmd == "check" {
		os.Exit(check(cfg, sem))
	}

	st, err := openStore(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	a := &app{cfg: cfg, sem: sem, store: st, limits: turnLimits{*maxSteps, *timeout}, logf: log.Printf, busy: map[string]bool{}}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: guard(os.Getenv("ANALYST_AUTH"), a.routes()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("analyst on http://%s, tools from %s", *listen, sem.url)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// check reports whether the hub profile and the models answer: 0 if they
// all do, 1 if not (the config itself loaded, or main would have exited 2).
func check(cfg *chatapi.Config, sem *semantic) int {
	ctx := context.Background()
	rc := 0
	if sess, err := sem.connect(ctx); err != nil {
		fmt.Println("hub:", err)
		rc = 1
	} else {
		tools, err := sem.tools(ctx, sess)
		sess.Close()
		if err != nil {
			fmt.Println("hub:", err)
			rc = 1
		} else {
			fmt.Printf("hub: %d tools from %s\n", len(tools), sem.url)
		}
	}
	for _, name := range cfg.ModelNames() {
		if err := chatapi.Ping(ctx, cfg.Models[name]); err != nil {
			fmt.Printf("model %s: %v\n", name, err)
			rc = 1
		} else {
			fmt.Printf("model %s: ok\n", name)
		}
	}
	return rc
}

// guard adds optional basic auth, and requires a custom header on every
// request that changes something, so other web pages can't forge them (a
// cross-origin request with a custom header needs a CORS preflight, which
// is never granted).
func guard(auth string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		if auth != "" {
			user, pass, _ := r.BasicAuth()
			if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(auth)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="analyst"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Analyst") == "" {
			http.Error(w, "missing X-Analyst header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.Handle("GET /api/models", handler(func(r *http.Request) (any, error) {
		return map[string]any{"models": a.cfg.ModelNames(), "default": a.cfg.Default}, nil
	}))
	mux.Handle("GET /api/conversations", handler(func(r *http.Request) (any, error) { return a.store.conversations(), nil }))
	mux.Handle("POST /api/conversations", handler(a.newConversation))
	mux.Handle("GET /api/conversations/{id}", handler(a.getConversation))
	mux.Handle("DELETE /api/conversations/{id}", handler(a.deleteConversation))
	mux.HandleFunc("POST /api/conversations/{id}/messages", a.message)
	mux.Handle("GET /api/reports", handler(func(r *http.Request) (any, error) { return a.store.reports(), nil }))
	mux.Handle("POST /api/reports", handler(a.saveReport))
	mux.Handle("GET /api/reports/{id}", handler(a.getReport))
	mux.Handle("DELETE /api/reports/{id}", handler(a.deleteReport))
	mux.Handle("POST /api/reports/{id}/run", handler(a.runReport))
	return mux
}

// --- conversations -------------------------------------------------------------

func (a *app) newConversation(r *http.Request) (any, error) {
	var in struct {
		Model string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Model == "" {
		in.Model = a.cfg.Default
	}
	if a.cfg.Models[in.Model] == nil {
		return nil, fail(http.StatusBadRequest, "unknown model %q", in.Model)
	}
	now := time.Now().UTC()
	c := &Conversation{ID: newID("c"), Model: in.Model, Created: now, Updated: now, Messages: []chatapi.Msg{}, Results: map[string]Result{}}
	if err := a.store.saveConversation(c); err != nil {
		return nil, err
	}
	return map[string]string{"id": c.ID}, nil
}

func (a *app) conv(r *http.Request) (*Conversation, error) {
	id := r.PathValue("id")
	c := (*Conversation)(nil)
	if validID(id) {
		c = a.store.conversation(id)
	}
	if c == nil {
		return nil, fail(http.StatusNotFound, "no conversation %q", id)
	}
	return c, nil
}

func (a *app) getConversation(r *http.Request) (any, error) {
	c, err := a.conv(r)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	busy := a.busy[c.ID]
	a.mu.Unlock()
	return map[string]any{"id": c.ID, "title": c.Title, "model": c.Model, "busy": busy, "items": items(c)}, nil
}

func (a *app) deleteConversation(r *http.Request) (any, error) {
	c, err := a.conv(r)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	busy := a.busy[c.ID]
	a.mu.Unlock()
	if busy {
		return nil, fail(http.StatusConflict, "this conversation is still answering; delete it when it is done")
	}
	return map[string]bool{"ok": true}, a.store.deleteConversation(c.ID)
}

// message runs one turn and streams its events as JSON lines. The turn goes
// on if the page goes away, and is saved as it runs. With edit, the text
// replaces that question (from 0) and everything after it is dropped.
func (a *app) message(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
		Edit *int   `json:"edit"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Text = strings.TrimSpace(in.Text); in.Text == "" {
		writeErr(w, fail(http.StatusBadRequest, "nothing to send"))
		return
	}
	// Marked busy before it is read, so the turn starts from the latest copy.
	id := r.PathValue("id")
	a.mu.Lock()
	if a.busy[id] {
		a.mu.Unlock()
		writeErr(w, fail(http.StatusConflict, "this conversation is still answering the previous question"))
		return
	}
	a.busy[id] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.busy, id)
		a.mu.Unlock()
	}()
	c, err := a.conv(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if in.Edit != nil && !c.rewind(*in.Edit) {
		writeErr(w, fail(http.StatusBadRequest, "no question %d to edit", *in.Edit))
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriter(w)
	emit := func(e event) {
		b, _ := json.Marshal(e)
		bw.Write(append(b, '\n'))
		bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	}
	a.turn(context.WithoutCancel(r.Context()), c, in.Text, emit)
	emit(event{Type: "done"})
}

// --- reports -------------------------------------------------------------------

func (a *app) saveReport(r *http.Request) (any, error) {
	var in struct {
		Title        string `json:"title"`
		Notes        string `json:"notes"`
		Conversation string `json:"conversation"`
		Steps        []struct {
			Call  string          `json:"call"` // a tool call id in the conversation
			Chart json.RawMessage `json:"chart"`
		} `json:"steps"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Title = strings.TrimSpace(in.Title); in.Title == "" {
		return nil, fail(http.StatusBadRequest, "a report needs a title")
	}
	if len(in.Steps) == 0 {
		return nil, fail(http.StatusBadRequest, "a report needs at least one query")
	}
	var c *Conversation
	if validID(in.Conversation) {
		c = a.store.conversation(in.Conversation)
	}
	if c == nil {
		return nil, fail(http.StatusNotFound, "no conversation %q", in.Conversation)
	}
	// The steps are the calls as the conversation made them, never as the
	// page sends them.
	calls := map[string]chatapi.Call{}
	for _, m := range c.Messages {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = tc
		}
	}
	rep := &Report{ID: newID("r"), Title: in.Title, Notes: strings.TrimSpace(in.Notes), Conversation: c.ID, Created: time.Now().UTC()}
	for _, s := range in.Steps {
		tc, ok := calls[s.Call]
		if !ok {
			return nil, fail(http.StatusBadRequest, "no tool call %q in the conversation", s.Call)
		}
		if res := c.Results[s.Call]; res.Error {
			return nil, fail(http.StatusBadRequest, "tool call %q failed; only queries that worked can be saved", s.Call)
		}
		if len(s.Chart) > 0 && !json.Valid(s.Chart) {
			return nil, fail(http.StatusBadRequest, "chart is not valid JSON")
		}
		rep.Steps = append(rep.Steps, Step{Tool: tc.Function.Name, Args: chatapi.ArgsJSON(tc.Function.Arguments), Chart: s.Chart})
	}
	if err := a.store.saveReport(rep); err != nil {
		return nil, err
	}
	return rep, nil
}

func (a *app) rep(r *http.Request) (*Report, error) {
	id := r.PathValue("id")
	var rep *Report
	if validID(id) {
		rep = a.store.report(id)
	}
	if rep == nil {
		return nil, fail(http.StatusNotFound, "no report %q", id)
	}
	return rep, nil
}

func (a *app) getReport(r *http.Request) (any, error) { return a.rep(r) }

func (a *app) deleteReport(r *http.Request) (any, error) {
	rep, err := a.rep(r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, a.store.deleteReport(rep.ID)
}

// runReport runs a report's tool calls again, in order, with no model.
func (a *app) runReport(r *http.Request) (any, error) {
	rep, err := a.rep(r)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.limits.Timeout)
	defer cancel()
	sess, err := a.sem.connect(ctx)
	if err != nil {
		return nil, fail(http.StatusBadGateway, "%v", err)
	}
	defer sess.Close()
	results := make([]Result, len(rep.Steps))
	for i, s := range rep.Steps {
		text, isErr := a.sem.call(ctx, sess, s.Tool, s.Args)
		results[i] = Result{Text: text, Error: isErr}
	}
	return map[string]any{"ran": time.Now().UTC(), "results": results}, nil
}

// --- helpers -------------------------------------------------------------------

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return &httpError{code, fmt.Sprintf(format, args...)}
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v); err != nil {
		return fail(http.StatusBadRequest, "bad request body: %v", err)
	}
	return nil
}

func handler(f func(*http.Request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := f(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	})
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var he *httpError
	if errors.As(err, &he) {
		code = he.code
	} else {
		log.Print(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
