package main

import (
	"strings"
	"testing"
	"time"
)

func TestJobs(t *testing.T) {
	p, err := loadProject("testdata/shop", target{name: "prod"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 2 {
		t.Fatalf("jobs = %d", len(p.jobs))
	}
	orders, nightly := p.jobs[0], p.jobs[1]
	if orders.Debounce.Duration != time.Minute || orders.MaxWait.Duration != 10*time.Minute {
		t.Errorf("defaults: debounce %s, max_wait %s", orders.Debounce, orders.MaxWait)
	}
	loc, _ := time.LoadLocation("UTC")
	if next := nightly.schedule.Next(time.Date(2026, 10, 20, 9, 0, 0, 0, loc)); !next.Equal(time.Date(2026, 10, 21, 3, 0, 0, 0, loc)) {
		t.Errorf("next nightly = %s", next)
	}

	tr := orders.On[0]
	for _, tc := range []struct {
		e    relayEvent
		want bool
	}{
		{relayEvent{Topic: "duckpipe.ingest", Type: "ok", Data: map[string]any{"table": "lake.raw.orders", "files": 3.0}}, true},
		{relayEvent{Topic: "duckpipe.ingest", Type: "failed", Data: map[string]any{"table": "lake.raw.orders"}}, false},
		{relayEvent{Topic: "duckpipe.ingest", Type: "ok", Data: map[string]any{"table": "lake.raw.events"}}, false},
		{relayEvent{Topic: "duckpipe.ingest", Type: "ok"}, false},
		{relayEvent{Topic: "duckpipe.poll", Type: "ok", Data: map[string]any{"table": "lake.raw.orders"}}, false},
	} {
		if got := tr.matches(tc.e); got != tc.want {
			t.Errorf("matches(%+v) = %v", tc.e, got)
		}
	}
	for _, tc := range []struct {
		tr   trigger
		e    relayEvent
		want bool
	}{
		{trigger{Topic: "duckpipe.*"}, relayEvent{Topic: "duckpipe.ingest", Type: "x"}, true},
		{trigger{Topic: "a.b, duckpipe.ingest", Type: "ok,failed"}, relayEvent{Topic: "duckpipe.ingest", Type: "failed"}, true},
		{trigger{Topic: "duckpipe.*"}, relayEvent{Topic: "duckpipex.ingest"}, false},
		{trigger{Topic: "x.y", Data: map[string]any{"n": 5}}, relayEvent{Topic: "x.y", Data: map[string]any{"n": 5.0}}, true},
		{trigger{Topic: "x.y", Data: map[string]any{"n": "5"}}, relayEvent{Topic: "x.y", Data: map[string]any{"n": 5.0}}, false},
	} {
		if got := tc.tr.matches(tc.e); got != tc.want {
			t.Errorf("%+v matches(%+v) = %v", tc.tr, tc.e, got)
		}
	}
}

func TestJobErrors(t *testing.T) {
	for jobs, want := range map[string]string{
		"jobs:\n  - name: a\n    select: x\n    schedule: '* * * * *'\n":                                                        "there is no model x",
		"jobs:\n  - name: a\n    select: m\n":                                                                                   "needs a schedule, on: triggers, or both",
		"jobs:\n  - name: a\n    select: m\n    schedule: '61 * * * *'\n":                                                       `schedule "61 * * * *"`,
		"jobs:\n  - name: a\n    select: m\n    schedule: '@daily'\n":                                                           `schedule "@daily"`,
		"jobs:\n  - name: a\n    select: m\n    on: [{topic: lakeshape.run}]\n":                                                 "lakeshape's own events never trigger jobs",
		"jobs:\n  - name: a\n    select: m\n    on: [{topic: '*'}]\n":                                                           "lakeshape's own events never trigger jobs",
		"jobs:\n  - name: a\n    select: m\n    on: [{type: ok}]\n":                                                             "topic is required",
		"jobs:\n  - name: a\n    select: m\n    on: [{topic: x.y, data: {a: [1]}}]\n":                                           "data.a: compare with a string",
		"jobs:\n  - name: a\n    select: m\n    on: [{topic: x.y}]\n    debounce: 15m\n":                                        "max_wait (10m0s) must be at least debounce (15m0s)",
		"jobs:\n  - name: a\n    select: m\n    schedule: '* * * * *'\n  - name: a\n    select: m\n    schedule: '* * * * *'\n": "name used twice",
		"jobs:\n  - name: a b\n    select: m\n":                                                                                 "name \"a b\"",
		"jobs:\n  - name: a\n    selects: m\n":                                                                                  "field selects not found",
	} {
		dir := writeProject(t, map[string]string{"models/m.sql": "select 1", "jobs.yml": jobs})
		if _, err := loadProject(dir, dev, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("jobs.yml:\n%s\nerror = %v, want %q", jobs, err, want)
		}
	}
}
