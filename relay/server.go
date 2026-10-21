package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed usage.txt
var usage string

// uiHTML is the read-only dashboard. It holds no data itself: the page asks
// for the token and reads the /v1 API with it.
//
//go:embed ui.html
var uiHTML []byte

const (
	maxWait      = 60 * time.Second // longest long-poll
	defaultLimit = 100
	maxLimit     = 1000
)

type server struct {
	token     string
	store     *store
	consumers *consumers
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, usage)
	})
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Write(uiHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.Handle("POST /v1/events", s.auth(s.publish))
	mux.Handle("GET /v1/events", s.auth(s.events))
	mux.Handle("GET /v1/topics", s.auth(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.store.topics()) }))
	mux.Handle("GET /v1/consumers", s.auth(s.listConsumers))
	mux.Handle("PUT /v1/consumers/{name}", s.auth(s.putConsumer))
	mux.Handle("DELETE /v1/consumers/{name}", s.auth(s.deleteConsumer))
	mux.Handle("GET /v1/consumers/{name}/next", s.auth(s.next))
	mux.Handle("POST /v1/consumers/{name}/ack", s.auth(s.ack))
	return mux
}

func (s *server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "unauthorized: send Authorization: Bearer $RELAY_TOKEN")
				return
			}
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decode reads a JSON request body into v. Requiring JSON means a web page
// elsewhere can't post without a CORS preflight, which is never granted.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxData+16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

func (s *server) publish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Topic string          `json:"topic"`
		Type  string          `json:"type"`
		RunID string          `json:"run_id"`
		Cause uint64          `json:"cause"`
		Key   string          `json:"key"`
		Data  json.RawMessage `json:"data"`
	}
	if !decode(w, r, &req) {
		return
	}
	e := Event{Topic: req.Topic, Type: req.Type, RunID: req.RunID, Cause: req.Cause, Key: req.Key, Data: req.Data}
	if err := e.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	stored, dup, err := s.store.add(e)
	if err != nil {
		log.Printf("publish: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not store event")
		return
	}
	code := http.StatusCreated
	if dup {
		code = http.StatusOK
	}
	writeJSON(w, code, stored)
}

type eventsResponse struct {
	Events []Event `json:"events"`
	Next   uint64  `json:"next"`
	Gap    bool    `json:"gap,omitempty"`
}

// poll calls read until it returns events or wait has passed; a new event
// wakes it up early.
func (s *server) poll(ctx context.Context, wait time.Duration, read func() (eventsResponse, error)) (eventsResponse, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		changed := s.store.changed() // taken before reading, so no event slips between
		resp, err := read()
		if err != nil || len(resp.Events) > 0 || wait <= 0 {
			return resp, err
		}
		select {
		case <-changed:
		case <-deadline.C:
			return resp, nil
		case <-ctx.Done():
			return resp, nil
		}
	}
}

func queryInt(q url.Values, name string, def, maxV int) (int, error) {
	v := q.Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, errors.New(name + " must be a non-negative integer")
	}
	return min(n, maxV), nil
}

func queryWait(q url.Values) (time.Duration, error) {
	v := q.Get("wait")
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, errors.New("wait must be a duration such as 30s")
	}
	return min(d, maxWait), nil
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := parseFilter(q.Get("topic"), q.Get("type"), q.Get("run_id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.Has("last") {
		n, err := queryInt(q, "last", 0, maxLimit)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		evs, next, err := s.store.last(n, f)
		if err != nil {
			log.Printf("read: %v", err)
			writeErr(w, http.StatusInternalServerError, "could not read events")
			return
		}
		writeJSON(w, 200, eventsResponse{Events: evs, Next: next})
		return
	}
	limit, err1 := queryInt(q, "limit", defaultLimit, maxLimit)
	wait, err2 := queryWait(q)
	after := s.store.latest()
	var err3 error
	if v := q.Get("after"); v != "" {
		after, err3 = strconv.ParseUint(v, 10, 64)
	}
	if err := errors.Join(err1, err2, err3); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.poll(r.Context(), wait, func() (eventsResponse, error) {
		evs, next, _, err := s.store.after(after, f, max(limit, 1))
		return eventsResponse{Events: evs, Next: next}, err
	})
	if err != nil {
		log.Printf("read: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not read events")
		return
	}
	writeJSON(w, 200, resp)
}

type consumerView struct {
	Name string `json:"name"`
	consumer
	Backlog int `json:"backlog"`
}

func (s *server) view(name string, c consumer) consumerView {
	return consumerView{Name: name, consumer: c, Backlog: s.store.count(c.Cursor, c.filter())}
}

func (s *server) listConsumers(w http.ResponseWriter, r *http.Request) {
	out := []consumerView{}
	for name, c := range s.consumers.all() {
		out = append(out, s.view(name, c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, out)
}

func (s *server) putConsumer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRE.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "consumer name must be 1-64 characters of A-Z a-z 0-9 _ . -")
		return
	}
	var req struct {
		Topic string          `json:"topic"`
		Type  string          `json:"type"`
		From  json.RawMessage `json:"from"` // "latest", "earliest" or an event ID
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Topic == "" {
		req.Topic = "*"
	}
	if _, err := parseFilter(req.Topic, req.Type, ""); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	latest := s.store.latest()
	var cursor *uint64
	if len(req.From) > 0 {
		from := strings.Trim(string(req.From), `"`)
		var c uint64
		switch from {
		case "latest":
			c = latest
		case "earliest":
			c = 0
		default:
			var err error
			if c, err = strconv.ParseUint(from, 10, 64); err != nil {
				writeErr(w, http.StatusBadRequest, `from must be "latest", "earliest" or an event ID`)
				return
			}
		}
		cursor = &c
	}
	c, err := s.consumers.put(name, req.Topic, req.Type, cursor, latest)
	if err != nil {
		log.Printf("consumers: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not save consumer")
		return
	}
	writeJSON(w, 200, s.view(name, c))
}

func (s *server) deleteConsumer(w http.ResponseWriter, r *http.Request) {
	if err := s.consumers.remove(r.PathValue("name")); errors.Is(err, errNoConsumer) {
		writeErr(w, http.StatusNotFound, err.Error())
	} else if err != nil {
		log.Printf("consumers: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not save consumers")
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) next(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.consumers.get(name); !ok {
		writeErr(w, http.StatusNotFound, "no such consumer: create it with PUT /v1/consumers/"+name)
		return
	}
	q := r.URL.Query()
	limit, err1 := queryInt(q, "limit", 1, maxLimit)
	wait, err2 := queryWait(q)
	if err := errors.Join(err1, err2); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.poll(r.Context(), wait, func() (eventsResponse, error) {
		c, ok := s.consumers.get(name)
		if !ok {
			return eventsResponse{}, errNoConsumer
		}
		evs, next, gap, err := s.store.after(c.Cursor, c.filter(), max(limit, 1))
		return eventsResponse{Events: evs, Next: next, Gap: gap}, err
	})
	if errors.Is(err, errNoConsumer) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		log.Printf("read: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not read events")
		return
	}
	writeJSON(w, 200, resp)
}

func (s *server) ack(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID uint64 `json:"id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.ID > s.store.latest() {
		writeErr(w, http.StatusBadRequest, "id is newer than the newest event")
		return
	}
	name := r.PathValue("name")
	c, err := s.consumers.ack(name, req.ID)
	if errors.Is(err, errNoConsumer) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		log.Printf("consumers: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not save consumer")
		return
	}
	writeJSON(w, 200, s.view(name, c))
}
