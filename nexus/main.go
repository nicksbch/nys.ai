// nexus: ask the server to do things, in a chat. A model works out the
// calls, using the tools of one mcp-hub profile. Read-only tools run right
// away; a call to a tool that changes something is shown as a card, and
// runs only when you press Start. A card that starts a long job (a
// download) follows it on relay until it ends.
//
// Usage:
//
//	nexus serve [-listen 127.0.0.1:8102] [-config config] [-hub http://127.0.0.1:8094] [-data data]
//	nexus check [-config config] [-hub http://127.0.0.1:8094]
//
// Secrets come from the environment: NEXUS_AUTH (the page's login, as
// user:pass), MCP_HUB_TOKEN, and the keys models.json names. Relay comes
// from RELAY_URL and RELAY_TOKEN, or /etc/relay-client.env.
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
	mcphub "mcp-hub/client"
	relay "relay/client"
)

//go:embed index.html
var indexHTML []byte

type app struct {
	ctx    context.Context // ends when nexus stops
	cfg    *chatapi.Config
	hub    *hubClient
	relay  *relay.Client // nil: jobs can't be followed
	store  *store
	limits turnLimits
	logf   func(string, ...any)
	wg     sync.WaitGroup // card calls and the jobs they follow

	mu   sync.Mutex
	busy map[string]bool // conversations with a turn running
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "serve" && os.Args[1] != "check") {
		fmt.Fprintln(os.Stderr, "usage: nexus serve|check [flags]   (nexus serve -h for the flags)")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8102", "address to serve the page on")
	configDir := fs.String("config", "config", "directory with models.json and prompt.md")
	hub := fs.String("hub", "http://127.0.0.1:8094", "mcp-hub")
	profile := fs.String("profile", "nexus", "mcp-hub profile: the tools nexus can use")
	dataDir := fs.String("data", "data", "where conversations are kept")
	maxSteps := fs.Int("max-steps", 8, "model calls per message")
	timeout := fs.Duration("timeout", 5*time.Minute, "longest a message may take to answer")
	fs.Parse(os.Args[2:])

	cfg, err := chatapi.LoadConfig(*configDir, os.Getenv, "/etc/nexus.env")
	if err != nil {
		fmt.Fprintln(os.Stderr, "nexus:", err)
		os.Exit(2)
	}
	h := &hubClient{url: strings.TrimRight(*hub, "/") + "/mcp/" + *profile, token: os.Getenv("MCP_HUB_TOKEN")}

	if cmd == "check" {
		os.Exit(check(cfg, h))
	}

	st, err := openStore(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bus := relay.FromEnv()
	if bus == nil {
		log.Print("relay is not configured (RELAY_URL): the jobs cards start can't be followed")
	}
	a := &app{ctx: ctx, cfg: cfg, hub: h, relay: bus, store: st, limits: turnLimits{*maxSteps, *timeout},
		logf: log.Printf, busy: map[string]bool{}}
	a.resume()

	srv := &http.Server{Addr: *listen, Handler: guard(os.Getenv("NEXUS_AUTH"), a.routes()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("nexus on http://%s, tools from %s", *listen, h.url)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	a.wg.Wait()
}

// check reports whether the hub profile and the models answer, and whether
// each model takes a tool call: 0 if all do, 1 if not (the config itself
// loaded, or main would have exited 2).
func check(cfg *chatapi.Config, h *hubClient) int {
	ctx := context.Background()
	rc := 0
	if sess, err := h.connect(ctx, "check"); err != nil {
		fmt.Println("hub:", err)
		rc = 1
	} else {
		ts, err := h.tools(ctx, sess)
		sess.Close()
		if err != nil {
			fmt.Println("hub:", err)
			rc = 1
		} else {
			var ro, act []string
			for _, d := range ts.defs {
				if mcphub.ReadOnly(ts.byName[d.Name]) {
					ro = append(ro, d.Name)
				} else {
					act = append(act, d.Name)
				}
			}
			fmt.Printf("hub: %s: run right away: %s; start from a card: %s\n", h.url, list(ro), list(act))
		}
	}
	for _, name := range cfg.ModelNames() {
		if err := chatapi.TestTools(ctx, cfg.Models[name]); err != nil {
			fmt.Printf("model %s: %v\n", name, err)
			rc = 1
		} else {
			fmt.Printf("model %s: ok\n", name)
		}
	}
	return rc
}

func list(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
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
				w.Header().Set("WWW-Authenticate", `Basic realm="nexus"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Nexus") == "" {
			http.Error(w, "missing X-Nexus header", http.StatusForbidden)
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
	mux.Handle("POST /api/conversations/{id}/cards/{card}/start", handler(func(r *http.Request) (any, error) {
		return a.cardAction(r, a.startCard)
	}))
	mux.Handle("POST /api/conversations/{id}/cards/{card}/cancel", handler(func(r *http.Request) (any, error) {
		return a.cardAction(r, a.cancelCard)
	}))
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
	cards := a.store.cardsOf(c.ID)
	active := false
	for _, card := range cards {
		active = active || card.State == cardCalling || card.State == cardRunning
	}
	return map[string]any{"id": c.ID, "title": c.Title, "model": c.Model, "busy": busy, "active": active, "items": items(c, cards)}, nil
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

// cardAction starts or cancels a card, and returns it as it now is.
func (a *app) cardAction(r *http.Request, f func(conv, id string) error) (any, error) {
	c, err := a.conv(r)
	if err != nil {
		return nil, err
	}
	id := r.PathValue("card")
	if !validID(id) {
		return nil, fail(http.StatusNotFound, "no card %q", id)
	}
	if err := f(c.ID, id); err != nil {
		return nil, err
	}
	return a.store.card(c.ID, id), nil
}

// message runs one turn and streams its events as JSON lines. The turn goes
// on if the page goes away, and is saved as it runs.
func (a *app) message(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
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
		writeErr(w, fail(http.StatusConflict, "this conversation is still answering the previous message"))
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
