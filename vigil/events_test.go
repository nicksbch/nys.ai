package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// The dashboard shows the last 24 hours even though more is kept in memory.
func TestStatusLast24h(t *testing.T) {
	m := testMonitor(t)
	w := httptest.NewRecorder()
	m.status(w, httptest.NewRequest("GET", "/api/status", nil))
	var got struct {
		Stats  []statView `json:"stats"`
		Events []Event    `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Add(-24 * time.Hour)
	for _, e := range got.Events {
		if e.Time.Before(day) {
			t.Errorf("event older than 24h on the dashboard: %+v", e)
		}
	}
	if len(got.Events) != 4 {
		t.Errorf("%d events, want the 4 of the last 24h", len(got.Events))
	}
	for _, v := range got.Stats {
		if v.App == "url-to-bucket" && (v.Count24 != 1 || v.Failures24 != 1) {
			t.Errorf("url-to-bucket 24h totals = %+v", v)
		}
	}
}

// The window is rebuilt from the log on restart, and capped at retention.
func TestEventWindowReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := openEventStore(dir, 90*24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, age := range []time.Duration{10 * 24 * time.Hour, 72 * time.Hour, time.Hour} {
		if err := s.add(Event{Time: now.Add(-age), App: "a", Kind: "k", Status: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	s.file.Close()

	s, err = openEventStore(dir, 90*24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stats, recent := s.snapshot()
	if len(recent) != 2 || stats[0].Count != 3 {
		t.Errorf("after restart: %d in memory (want 2), %d in totals (want 3)", len(recent), stats[0].Count)
	}
	s.file.Close()

	s, err = openEventStore(dir, 48*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.file.Close()
	if _, recent := s.snapshot(); s.window != 48*time.Hour || len(recent) != 1 {
		t.Errorf("window %s, %d in memory; want 48h and 1", s.window, len(recent))
	}
}
