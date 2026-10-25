package main

// What is running now, from relay alone. Every service that does work
// publishes "started" and then "ok" or "failed" with the same run ID, so a
// run is running while its "started" has no result, and waiting while its
// "requested" has no "started".

import (
	"sort"
	"strings"
	"time"
)

const (
	// noResultAfter is how long a run may go before atlas stops calling it
	// running: a service killed mid-run never publishes the result.
	noResultAfter = 6 * time.Hour
	// endedWindow is how long a run that ended stays on the page.
	endedWindow = 15 * time.Minute
	// lookBack is how old a start may be and still count: older ones would be
	// looked up again on every refresh, to show only that nothing came back.
	lookBack = 24 * time.Hour
)

type run struct {
	RunID   string     `json:"run_id"`
	Topic   string     `json:"topic"`
	Service string     `json:"service"`
	Name    string     `json:"name"`
	State   string     `json:"state"` // running, waiting, ended, no_result
	Since   time.Time  `json:"since"` // when it started, or was asked for
	Ended   *time.Time `json:"ended,omitempty"`
	Result  string     `json:"result,omitempty"` // ok, failed
	Error   string     `json:"error,omitempty"`
	Done    int        `json:"done,omitempty"`  // lakeshape: models finished
	Total   int        `json:"total,omitempty"` // lakeshape: models selected
	Nodes   []string   `json:"nodes,omitempty"`

	start event // the "started", or else the "requested"
}

// runOf is one run's events: its newest "started", its first "requested",
// and its result.
type runOf struct {
	started, requested, end *event
	events                  []event
}

// collect groups every event we have by run ID, without duplicates.
func collect(re *runEvents) map[string]*runOf {
	byRun := map[string]*runOf{}
	seen := map[uint64]bool{}
	add := func(ev event) {
		if ev.RunID == "" || seen[ev.ID] {
			return
		}
		seen[ev.ID] = true
		r := byRun[ev.RunID]
		if r == nil {
			r = &runOf{}
			byRun[ev.RunID] = r
		}
		r.events = append(r.events, ev)
	}
	for _, evs := range [][]event{re.Starts, re.Ends} {
		for _, ev := range evs {
			add(ev)
		}
	}
	for _, evs := range re.ByRun {
		for _, ev := range evs {
			add(ev)
		}
	}
	for _, r := range byRun {
		sort.Slice(r.events, func(i, j int) bool { return r.events[i].ID > r.events[j].ID })
		r.pair()
	}
	return byRun
}

// pair finds the run's start and its result. A result counts only on the
// start's own topic: other services' events can carry the run ID (mcp-hub's
// calls carry foreman's, lakeshape's models carry the run's). A run that was
// only requested ends with a result from the same service, such as
// lakeshape.run failing a trigger for a job that doesn't exist.
func (r *runOf) pair() {
	for i := range r.events {
		ev := &r.events[i]
		switch {
		case ev.Type == "started" && r.started == nil:
			r.started = ev
		case ev.Type == "requested":
			r.requested = ev // newest first: the last one seen is the first asked
		}
	}
	for i := range r.events {
		ev := &r.events[i]
		if ev.Type != "ok" && ev.Type != "failed" {
			continue
		}
		if r.started != nil && ev.Topic == r.started.Topic ||
			r.started == nil && r.requested != nil && app(ev.Topic) == app(r.requested.Topic) {
			r.end = ev
			return
		}
	}
}

// first is the event that began the run: its "started", or else its
// "requested".
func (r *runOf) first() *event {
	if r.started != nil {
		return r.started
	}
	return r.requested
}

// unanswered lists the run IDs among starts in the last lookBack that have
// no result in ends: those are looked up whole, by run ID.
func unanswered(starts, ends []event, now time.Time) []string {
	byRun := collect(&runEvents{Starts: starts, Ends: ends})
	var out []string
	for id, r := range byRun {
		if first := r.first(); first != nil && now.Sub(first.Time) <= lookBack && r.end == nil {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// runs works out each run's state at now. Runs that ended before the
// window are left out.
func runs(re *runEvents, now time.Time) []*run {
	var out []*run
	for id, r := range collect(re) {
		first := r.first()
		if first == nil || now.Sub(first.Time) > lookBack {
			continue // only results, from runs that started before what we read
		}
		x := &run{RunID: id, Topic: first.Topic, Service: app(first.Topic), Name: runName(*first), Since: first.Time, start: *first}
		switch {
		case r.end != nil:
			if now.Sub(r.end.Time) > endedWindow {
				continue
			}
			t := r.end.Time
			x.State, x.Ended, x.Result = "ended", &t, r.end.Type
			if s, ok := r.end.Data["error"].(string); ok {
				x.Error, _, _ = strings.Cut(s, "\n")
			}
		case now.Sub(first.Time) > noResultAfter:
			x.State = "no_result"
		case r.started != nil:
			x.State = "running"
		default:
			x.State = "waiting"
		}
		if first.Topic == "lakeshape.run" {
			x.Total = asInt(first.Data["models"])
			for _, ev := range r.events {
				if ev.Topic == "lakeshape.model" && ev.Type != "started" {
					x.Done++
				}
			}
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since.Equal(out[j].Since) {
			return out[i].RunID < out[j].RunID
		}
		return out[i].Since.After(out[j].Since)
	})
	return out
}

// runName is what a run is working on: the event's "name", which most
// services set, or what lakeshape's events carry instead.
func runName(ev event) string {
	if s, ok := ev.Data["name"].(string); ok && s != "" {
		return s
	}
	if s, ok := ev.Data["url"].(string); ok && s != "" { // a download asked for
		b, _ := ev.Data["bucket"].(string)
		k, _ := ev.Data["key"].(string)
		return s + " -> s3://" + b + "/" + k
	}
	if s, ok := ev.Data["job"].(string); ok && s != "" {
		return "job " + s
	}
	if jobs, ok := ev.Data["jobs"].([]any); ok && len(jobs) > 0 {
		var names []string
		for _, j := range jobs {
			if s, ok := j.(string); ok {
				names = append(names, s)
			}
		}
		return "jobs " + strings.Join(names, ", ")
	}
	return ev.Topic
}

func asInt(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// placeRuns gives each run the graph nodes it belongs to, by the same
// matching as each node's last event.
func placeRuns(rs []*run, g *graph) {
	if g == nil {
		return
	}
	for _, r := range rs {
		for _, n := range g.Nodes {
			if ownEvent(n, r.start) {
				r.Nodes = append(r.Nodes, n.ID)
			}
		}
	}
}
