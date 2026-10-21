package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustAdd(t *testing.T, s *store, e Event) Event {
	t.Helper()
	if err := e.validate(); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.add(e)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestStoreReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, s, Event{Topic: "a.download", Type: "started", RunID: "r1"})
	mustAdd(t, s, Event{Topic: "a.download", Type: "ok", RunID: "r1", Data: json.RawMessage(`{"bytes": 5}`)})
	mustAdd(t, s, Event{Topic: "b.ingest", Type: "failed", Key: "k1"})
	s.close()

	s, err = openStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.latest() != 3 {
		t.Fatalf("latest = %d, want 3", s.latest())
	}
	f, _ := parseFilter("a.*", "ok", "")
	evs, next, _, err := s.after(0, f, 10)
	if err != nil || len(evs) != 1 || evs[0].ID != 2 || string(evs[0].Data) != `{"bytes":5}` || next != 3 {
		t.Fatalf("after = %+v, %d, %v", evs, next, err)
	}
	// The idempotency key survives the restart.
	if _, dup, _ := s.add(Event{Topic: "b.ingest", Type: "failed", Key: "k1"}); !dup {
		t.Fatal("repeated key was stored again after restart")
	}
	if e := mustAdd(t, s, Event{Topic: "a.download", Type: "ok"}); e.ID != 4 {
		t.Fatalf("new id = %d, want 4", e.ID)
	}
}

func TestStoreIdempotency(t *testing.T) {
	s, _ := openStore(t.TempDir(), time.Hour)
	defer s.close()
	a := mustAdd(t, s, Event{Topic: "a.b", Type: "ok", Key: "x"})
	b, dup, err := s.add(Event{Topic: "a.b", Type: "ok", Key: "x"})
	if err != nil || !dup || b.ID != a.ID {
		t.Fatalf("dup = %v, id %d vs %d, err %v", dup, b.ID, a.ID, err)
	}
	if s.latest() != 1 {
		t.Fatalf("latest = %d, want 1", s.latest())
	}
}

func TestStoreTornLine(t *testing.T) {
	dir := t.TempDir()
	s, _ := openStore(dir, time.Hour)
	mustAdd(t, s, Event{Topic: "a.b", Type: "ok"})
	path := s.segs[0].path
	s.close()

	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"id":2,"time":"2026-`)
	f.Close()

	s, err := openStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if e := mustAdd(t, s, Event{Topic: "a.b", Type: "ok"}); e.ID != 2 {
		t.Fatalf("id after recovery = %d, want 2", e.ID)
	}
	evs, _, _, err := s.after(0, filter{}, 10)
	if err != nil || len(evs) != 2 {
		t.Fatalf("after recovery: %d events, %v", len(evs), err)
	}
}

func TestStoreRetention(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "events")
	os.MkdirAll(segDir, 0o750)
	old := time.Now().UTC().Add(-72 * time.Hour)
	mid := time.Now().UTC().Add(-48 * time.Hour)
	for i, tm := range []time.Time{old, mid} {
		b, _ := json.Marshal(Event{ID: uint64(i + 1), Time: tm, Topic: "a.b", Type: "ok"})
		os.WriteFile(filepath.Join(segDir, tm.Format(dayFormat)+".jsonl"), append(b, '\n'), 0o640)
	}

	// Both segments are older than a day, but the newest is always kept.
	s, err := openStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if len(s.segs) != 1 || s.segs[0].day != mid.Format(dayFormat) {
		t.Fatalf("segments = %d, want only %s", len(s.segs), mid.Format(dayFormat))
	}
	if _, err := os.Stat(filepath.Join(segDir, old.Format(dayFormat)+".jsonl")); !os.IsNotExist(err) {
		t.Fatal("old segment file still exists")
	}
	evs, _, gap, _ := s.after(0, filter{}, 10)
	if len(evs) != 1 || evs[0].ID != 2 || !gap {
		t.Fatalf("after retention: %+v gap=%v", evs, gap)
	}
	if e := mustAdd(t, s, Event{Topic: "a.b", Type: "ok"}); e.ID != 3 {
		t.Fatalf("id = %d, want 3", e.ID)
	}
}

func TestFilter(t *testing.T) {
	m := &meta{topic: "url-to-bucket.download", typ: "ok", runID: "r1"}
	for _, c := range []struct {
		topic, typ, run string
		want            bool
	}{
		{"", "", "", true},
		{"*", "", "", true},
		{"url-to-bucket.*", "", "", true},
		{"url-to-bucket.download", "ok,failed", "r1", true},
		{"url-to-bucket", "", "", false},
		{"url-to.*", "", "", false},
		{"x.y,url-to-bucket.download", "", "", true},
		{"", "failed", "", false},
		{"", "", "r2", false},
	} {
		f, err := parseFilter(c.topic, c.typ, c.run)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.match(m); got != c.want {
			t.Errorf("filter(%q, %q, %q) = %v, want %v", c.topic, c.typ, c.run, got, c.want)
		}
	}
	if _, err := parseFilter("Bad Topic", "", ""); err == nil {
		t.Error("bad topic filter accepted")
	}
}

func TestValidate(t *testing.T) {
	for _, e := range []Event{
		{Topic: "", Type: "ok"},
		{Topic: "A.b", Type: "ok"},
		{Topic: "a.b", Type: ""},
		{Topic: "a.b", Type: "ok", Data: json.RawMessage(`[1]`)},
		{Topic: "a.b", Type: "ok", RunID: "has space"},
	} {
		if err := e.validate(); err == nil {
			t.Errorf("validate(%+v) accepted", e)
		}
	}
	e := Event{Topic: "a.b", Type: "ok", Data: json.RawMessage(`null`)}
	if err := e.validate(); err != nil || e.Data != nil {
		t.Errorf("null data: %v, %s", err, e.Data)
	}
}
