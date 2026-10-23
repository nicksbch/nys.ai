package main

import (
	"path/filepath"
	"testing"
)

// The config that deploy.sh installs on the server.
func TestDeployedConfig(t *testing.T) {
	c, err := loadConfig("preprocessor.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Jobs) == 0 {
		t.Errorf("config = %+v", c)
	}
}

func TestMatches(t *testing.T) {
	j := &job{Name: "x", Bucket: "b", Prefix: "uk/crime/", To: "uk/crime/extracted"}
	if err := (&config{Jobs: []*job{j}}).check(); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]bool{
		"uk/crime/2026-04.zip":                    true,
		"uk/crime/old/2025-01.zip":                true,
		"uk/crime/2026-04.csv":                    false,
		"uk/crime/":                               false,
		"uk/other/2026-04.zip":                    false,
		"uk/crime/extracted/2026-04/nested.zip":   false, // written by the job itself
		"uk/crime/extracted/2026-04/a-street.csv": false,
	} {
		if got := j.matches(key); got != want {
			t.Errorf("matches(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestEntryKey(t *testing.T) {
	j := &job{To: "uk/crime/extracted/"}
	if k, err := j.entryKey("2026-01/2026-01-kent-street.csv"); err != nil || k != "uk/crime/extracted/2026-01/2026-01-kent-street.csv" {
		t.Errorf("entryKey = %q, %v", k, err)
	}
	for _, bad := range []string{"", "../x.csv", "/etc/x", "a/../../x", "a//b", `a\b`, "..", "./a"} {
		if _, err := j.entryKey(bad); err == nil {
			t.Errorf("entryKey(%q): want an error", bad)
		}
	}
}

func TestCheck(t *testing.T) {
	for name, j := range map[string]*job{
		"no to":        {Name: "a", Bucket: "b", Prefix: "p/"},
		"prefix in to": {Name: "a", Bucket: "b", Prefix: "out/in/", To: "out/"},
		"to not clean": {Name: "a", Bucket: "b", To: "a/../b/"},
		"absolute to":  {Name: "a", Bucket: "b", To: "/out/"},
		"no bucket":    {Name: "a", To: "out/"},
		"bad pattern":  {Name: "a", Bucket: "b", To: "out/", Pattern: "["},
	} {
		if err := (&config{Jobs: []*job{j}}).check(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestRetry(t *testing.T) {
	e := &engine{cfg: &config{}, state: &store{file: filepath.Join(t.TempDir(), "state.json")}}
	for _, r := range []record{
		{Job: "a", Key: "1.zip", ETag: "e1", Status: "ok"},
		{Job: "a", Key: "2.zip", ETag: "e2", Status: "failed"},
		{Job: "b", Key: "3.zip", ETag: "e3", Status: "failed"},
	} {
		if err := e.state.add(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.retry("a"); err != nil {
		t.Fatal(err)
	}
	rs, err := e.state.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Key != "1.zip" || rs[1].Key != "3.zip" {
		t.Errorf("after retry a: %+v", rs)
	}
}

func TestLatest(t *testing.T) {
	rs := latest([]record{
		{Job: "a", Bucket: "b", Key: "1.zip", ETag: "old", Status: "ok"},
		{Job: "a", Bucket: "b", Key: "1.zip", ETag: "new", Status: "failed"},
	})
	if len(rs) != 1 || rs[0].Status != "failed" {
		t.Errorf("latest = %+v", rs)
	}
}
