package main

// The web page served by "preprocessor run": each job's archives as the last
// poll found them, the history in state.json, and buttons to poll now, to
// unzip an archive again and to remove records of past attempts. Jobs come
// from the config and can't be changed here.

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// serve runs the page on addr until ctx ends. auth is "user:pass" for basic
// auth, or empty for none.
func (e *engine) serve(ctx context.Context, addr, auth string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})
	mux.HandleFunc("GET /api/state", e.apiState)
	mux.HandleFunc("POST /api/poll", e.apiPoll)
	mux.HandleFunc("POST /api/redo", e.apiRedo)
	mux.HandleFunc("POST /api/remove", e.apiRemove)
	srv := &http.Server{Addr: addr, Handler: guard(auth, mux), ReadHeaderTimeout: 10 * time.Second}
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
				w.Header().Set("WWW-Authenticate", `Basic realm="preprocessor"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Preprocessor") == "" {
			http.Error(w, "missing X-Preprocessor header", http.StatusForbidden)
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

// stateView is everything the page shows.
type stateView struct {
	Poll    string       `json:"poll"`
	Busy    *unzipping   `json:"busy"`
	Jobs    []jobView    `json:"jobs"`
	History []historyRow `json:"history"` // newest first
}

type jobView struct {
	Name     string        `json:"name"`
	Bucket   string        `json:"bucket"`
	Prefix   string        `json:"prefix"`
	Pattern  string        `json:"pattern"`
	To       string        `json:"to"`
	Polled   bool          `json:"polled"` // listed at least once since the service started
	PolledAt time.Time     `json:"polled_at"`
	Error    string        `json:"error,omitempty"` // of the last poll
	Archives []archiveView `json:"archives"`        // oldest first, as they are unzipped
}

type archiveView struct {
	object
	// "pending" (unzipped on the next poll), "unzipping", "ok" or "failed".
	Status string  `json:"status"`
	Last   *record `json:"last,omitempty"` // the attempt that counts for its current ETag
}

type historyRow struct {
	record
	// Current: this attempt keeps the archive, still in the bucket with this
	// ETag, from being unzipped again. Removing it would unzip it again, so
	// the page offers "Extract again" instead.
	Current bool `json:"current"`
}

// view joins the config, the last listings, the archive being unzipped and
// the records.
func view(cfg *config, listed map[string]listing, busy *unzipping, rs []record) stateView {
	last := map[string]*record{} // id -> newest attempt that counts
	for i := range rs {
		if !rs[i].Redone {
			last[rs[i].id()] = &rs[i]
		}
	}
	current := map[*record]bool{}
	v := stateView{Poll: cfg.Poll.String(), Busy: busy, Jobs: []jobView{}, History: []historyRow{}}
	for _, j := range cfg.Jobs {
		l, polled := listed[j.Name]
		jv := jobView{Name: j.Name, Bucket: j.Bucket, Prefix: j.Prefix, Pattern: j.Pattern, To: j.To,
			Polled: polled && !l.At.IsZero(), PolledAt: l.At, Error: l.Err, Archives: []archiveView{}}
		for _, o := range l.Archives {
			a := archiveView{object: o, Status: "pending"}
			if r := last[record{Job: j.Name, Bucket: j.Bucket, Key: o.Key, ETag: o.ETag}.id()]; r != nil {
				a.Status, a.Last = r.Status, r
				current[r] = true
			}
			if busy != nil && busy.Job == j.Name && busy.Key == o.Key {
				a.Status = "unzipping"
			}
			jv.Archives = append(jv.Archives, a)
		}
		v.Jobs = append(v.Jobs, jv)
	}
	for i := len(rs) - 1; i >= 0; i-- {
		v.History = append(v.History, historyRow{record: rs[i], Current: current[&rs[i]]})
	}
	return v
}

func (e *engine) snapshot() (map[string]listing, *unzipping) {
	e.mu.Lock()
	defer e.mu.Unlock()
	listed := make(map[string]listing, len(e.listed))
	for k, v := range e.listed {
		listed[k] = v
	}
	var busy *unzipping
	if e.busy != nil {
		b := *e.busy
		busy = &b
	}
	return listed, busy
}

func (e *engine) apiState(w http.ResponseWriter, r *http.Request) {
	rs, err := e.state.load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	listed, busy := e.snapshot()
	writeJSON(w, http.StatusOK, view(e.cfg, listed, busy, rs))
}

// pollNow starts the next poll as soon as the current one, if any, is over.
func (e *engine) pollNow() {
	select {
	case e.poke <- struct{}{}:
	default: // one is already asked for
	}
}

func (e *engine) apiPoll(w http.ResponseWriter, r *http.Request) {
	e.pollNow()
	writeJSON(w, http.StatusAccepted, map[string]any{})
}

func (e *engine) job(name string) *job {
	for _, j := range e.cfg.Jobs {
		if j.Name == name {
			return j
		}
	}
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(v); err != nil {
		return fmt.Errorf("body must be JSON: %w", err)
	}
	return nil
}

// apiRedo marks an archive's records redone and polls, so it is unzipped
// again. Entries that haven't changed are skipped as usual.
func (e *engine) apiRedo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Job string `json:"job"`
		Key string `json:"key"`
	}
	if err := decode(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	j := e.job(req.Job)
	if j == nil || !j.matches(req.Key) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not an archive of job %q", req.Key, req.Job))
		return
	}
	n, err := e.state.redo(j.Name, j.Bucket, req.Key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("%s: page: unzip s3://%s/%s again (%d records marked redone)", j.Name, j.Bucket, req.Key, n)
	e.pollNow()
	writeJSON(w, http.StatusOK, map[string]int{"redone": n})
}

// apiRemove deletes the record of one past attempt. The attempt that keeps an
// archive still in the bucket from being unzipped again can't be removed:
// that is "Extract again".
func (e *engine) apiRemove(w http.ResponseWriter, r *http.Request) {
	var req record
	if err := decode(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := e.removable(req); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	found, err := e.state.remove(req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("no such record; it may have been removed already"))
		return
	}
	log.Printf("%s: page: removed the %s record of s3://%s/%s from %s", req.Job, req.Status, req.Bucket, req.Key, req.At.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{})
}

// removable reports why the record can't be removed, if it can't.
func (e *engine) removable(rec record) error {
	j := e.job(rec.Job)
	if j == nil || j.Bucket != rec.Bucket || rec.Redone {
		return nil // a job since removed from the config, or an attempt already redone
	}
	listed, busy := e.snapshot()
	l, ok := listed[j.Name]
	if !ok || l.At.IsZero() {
		return errors.New("wait for the first poll: until then it isn't known whether this archive is still in the bucket")
	}
	rs, err := e.state.load()
	if err != nil {
		return err
	}
	for _, h := range view(e.cfg, map[string]listing{j.Name: l}, busy, rs).History {
		if h.Current && h.id() == rec.id() && h.At.Equal(rec.At) {
			return errors.New("this archive is still in the bucket and removing its record would unzip it again: use \"Extract again\" for that")
		}
	}
	return nil
}
