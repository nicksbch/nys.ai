package main

// The web page and the JSON API behind it, both behind a password (basic
// auth, FOREMAN_AUTH). GET / and /healthz stay open.

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

//go:embed ui.html
var uiHTML []byte

func (e *engine) routes(ctx context.Context, auth string) http.Handler {
	mux := http.NewServeMux()
	open := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	private := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, basicAuth(auth, h)) }

	open("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	open("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, usageText)
	})
	private("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(uiHTML)
	})

	private("GET /api/tasks", e.apiTasks)
	private("GET /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		t := e.store.get(r.PathValue("id"))
		if t == nil {
			apiError(w, http.StatusNotFound, "no such task")
			return
		}
		var output []string
		if tt := e.defs.Types[t.Type]; tt != nil {
			output = tt.Output
		}
		writeJSON(w, map[string]any{"task": t, "output": output})
	})
	private("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, t := range e.store.find(func(t *Task) bool {
			return slices.ContainsFunc(t.Runs, func(run *RunInfo) bool { return run.ID == id })
		}) {
			for _, run := range t.Runs {
				if run.ID == id {
					recs, err := e.store.readTranscript(run)
					if err != nil {
						apiError(w, http.StatusInternalServerError, "reading the transcript: "+err.Error())
						return
					}
					writeJSON(w, map[string]any{"task": t.ID, "run": run, "records": recs})
					return
				}
			}
		}
		apiError(w, http.StatusNotFound, "no such run")
	})
	private("POST /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Type, Input string }
		if !readJSON(w, r, &in) {
			return
		}
		t, err := e.createUserTask(ctx, in.Type, in.Input)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"task": t})
	})
	private("POST /api/tasks/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Status string }
		if readJSON(w, r, &in) {
			e.reply(w, r, e.setStatus(r.PathValue("id"), in.Status))
		}
	})
	private("POST /api/tasks/{id}/verdict", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Verdict, Note string }
		if readJSON(w, r, &in) {
			e.reply(w, r, e.setVerdict(r.PathValue("id"), in.Verdict, in.Note))
		}
	})
	private("POST /api/tasks/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Agent, Model string }
		if readJSON(w, r, &in) {
			e.reply(w, r, e.rerun(ctx, r.PathValue("id"), in.Agent, in.Model))
		}
	})
	private("GET /api/agents", e.apiAgents)
	private("GET /api/types", e.apiTypes)
	return mux
}

// reply answers an action with the task as it now is, or the error.
func (e *engine) reply(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		code := http.StatusConflict
		if strings.HasPrefix(err.Error(), "no task") {
			code = http.StatusNotFound
		} else if strings.Contains(err.Error(), "must be") || strings.HasPrefix(err.Error(), "no ") {
			code = http.StatusBadRequest
		}
		apiError(w, code, err.Error())
		return
	}
	writeJSON(w, map[string]any{"task": e.store.get(r.PathValue("id"))})
}

// apiTasks lists tasks, newest first. status takes a comma-separated list;
// "inbox" means answered and failed: the tasks waiting for you.
func (e *engine) apiTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var statuses []string
	for _, s := range strings.Split(q.Get("status"), ",") {
		switch s {
		case "":
		case "inbox":
			statuses = append(statuses, statusAnswered, statusFailed)
		case "open":
			statuses = append(statuses, statusCreated, statusRunning, statusAnswered, statusFailed)
		default:
			statuses = append(statuses, s)
		}
	}
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 1000)
	}
	typ, agent, verdict := q.Get("type"), q.Get("agent"), q.Get("verdict")
	tasks := e.store.find(func(t *Task) bool {
		if len(statuses) > 0 && !slices.Contains(statuses, t.Status) {
			return false
		}
		if typ != "" && t.Type != typ {
			return false
		}
		if agent != "" && !slices.ContainsFunc(t.Runs, func(run *RunInfo) bool { return run.Agent == agent }) {
			return false
		}
		switch verdict {
		case "":
		case "none":
			return t.Verdict == ""
		default:
			return t.Verdict == verdict
		}
		return true
	})
	if tasks == nil {
		tasks = []*Task{} // an empty list, not null
	}
	total := len(tasks)
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	for _, t := range tasks {
		t.Input = "" // can be long; the task page has it
	}
	writeJSON(w, map[string]any{"tasks": tasks, "total": total})
}

type counts struct {
	Tasks    int `json:"tasks"`
	Answered int `json:"answered"`
	Failed   int `json:"failed"`
	Open     int `json:"open"`
	Good     int `json:"good"`
	Bad      int `json:"bad"`
}

func (c *counts) add(t *Task) {
	c.Tasks++
	if !closed(t.Status) {
		c.Open++
	}
	if t.Result != nil {
		c.Answered++
	}
	if slices.ContainsFunc(t.Runs, func(r *RunInfo) bool { return r.Status == "failed" }) {
		c.Failed++
	}
	switch t.Verdict {
	case "good":
		c.Good++
	case "bad":
		c.Bad++
	}
}

func (e *engine) apiAgents(w http.ResponseWriter, r *http.Request) {
	type agentView struct {
		*Agent
		Timeout string `json:"timeout"`
		Counts  counts `json:"counts"`
	}
	byAgent := map[string]*counts{}
	for _, t := range e.store.find(func(*Task) bool { return true }) {
		seen := map[string]bool{}
		for _, run := range t.Runs {
			if !seen[run.Agent] {
				seen[run.Agent] = true
				if byAgent[run.Agent] == nil {
					byAgent[run.Agent] = &counts{}
				}
				byAgent[run.Agent].add(t)
			}
		}
	}
	out := []agentView{}
	for _, a := range e.defs.Agents {
		v := agentView{Agent: a, Timeout: a.Timeout.String()}
		if c := byAgent[a.Name]; c != nil {
			v.Counts = *c
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	models := make([]string, 0, len(e.defs.Models))
	for name := range e.defs.Models {
		models = append(models, name)
	}
	sort.Strings(models)
	writeJSON(w, map[string]any{"agents": out, "models": models})
}

func (e *engine) apiTypes(w http.ResponseWriter, r *http.Request) {
	type typeView struct {
		*TaskType
		Every  string `json:"every,omitempty"`
		Counts counts `json:"counts"`
	}
	byType := map[string]*counts{}
	for _, t := range e.store.find(func(*Task) bool { return true }) {
		if byType[t.Type] == nil {
			byType[t.Type] = &counts{}
		}
		byType[t.Type].add(t)
	}
	out := []typeView{}
	for _, tt := range e.defs.Types {
		v := typeView{TaskType: tt}
		if tt.Every > 0 {
			v.Every = tt.Every.String()
		}
		if c := byType[tt.Name]; c != nil {
			v.Counts = *c
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, map[string]any{"types": out})
}

// --- helpers ------------------------------------------------------------------

func basicAuth(auth string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(auth)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="foreman"`)
			apiError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readJSON decodes a request body. Requiring JSON means a page on another
// site can't post here without a CORS preflight, which is never granted.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		apiError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
