package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	relay "relay/client"
)

// service runs the project's jobs on the prod target: on their schedules and
// when relay events match their triggers. One run at a time; see SPEC.md.
type service struct {
	dir string
	lo  lakeOptions
	out io.Writer
	pub *publisher
	api *relayAPI // nil without relay

	base  *project
	envs  map[string]*env // by job vars ("" for none): jobs with vars render differently
	jobs  map[string]*job
	order []string // job names, as in jobs.yml
}

type env struct {
	p *project
	l *lake
}

// request asks for one job to run.
type request struct {
	job         string
	fullRefresh bool
	runID       string   // set by `lakeshape trigger`: the job runs alone, with this run ID
	events      []uint64 // relay events it answers, acked once it has run
}

// window collects a job's matching events until they have been quiet for
// debounce, or max_wait has passed since the first.
type window struct {
	first, last time.Time
	events      []uint64
}

type eventBatch struct {
	events []relayEvent
	next   uint64 // read position after them
}

func varsKey(vars map[string]any) string {
	if len(vars) == 0 {
		return ""
	}
	b, _ := json.Marshal(vars) // map keys come out sorted
	return string(b)
}

// load reads the project and jobs, and opens the lake for each set of job vars.
func (s *service) load(ctx context.Context) error {
	prod := target{name: "prod"}
	p, err := loadProject(s.dir, prod, nil)
	if err != nil {
		return err
	}
	if len(p.jobs) == 0 {
		return fmt.Errorf("jobs.yml has no jobs: nothing to serve")
	}
	envs := map[string]*env{}
	jobs := map[string]*job{}
	var order []string
	for _, j := range append([]*job{nil}, p.jobs...) {
		key := ""
		if j != nil {
			jobs[j.Name] = j
			order = append(order, j.Name)
			key = varsKey(j.Vars)
		}
		if envs[key] != nil {
			continue
		}
		jp := p
		if key != "" {
			if jp, err = loadProject(s.dir, prod, j.Vars); err != nil {
				return fmt.Errorf("job %s vars: %w", j.Name, err)
			}
		}
		l, err := openLake(ctx, jp, s.lo)
		if err != nil {
			return err
		}
		envs[key] = &env{jp, l}
	}
	s.base, s.envs, s.jobs, s.order = p, envs, jobs, order
	return nil
}

// filter returns the relay filter that covers every job's triggers, and
// lakeshape trigger requests.
func (s *service) filter() (topic, typ string) {
	topics := []string{"lakeshape.trigger"}
	types := []string{"requested"}
	anyType := false
	for _, name := range s.order {
		for _, t := range s.jobs[name].On {
			topics = union(topics, splitList(t.Topic))
			if t.Type == "" {
				anyType = true
			}
			types = union(types, splitList(t.Type))
		}
	}
	if anyType {
		return strings.Join(topics, ","), ""
	}
	return strings.Join(topics, ","), strings.Join(types, ",")
}

// lastRuns returns when each job last ran, from run_history.
func (s *service) lastRuns(ctx context.Context) (map[string]time.Time, error) {
	l := s.envs[""].l
	rows, err := l.query(ctx, `SELECT job, strftime(max(started_at) AT TIME ZONE 'UTC', '%Y-%m-%dT%H:%M:%S.%fZ') AS last
		FROM (SELECT unnest(jobs) AS job, started_at FROM `+l.hist+`) GROUP BY job`)
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, r := range rows {
		t, err := time.Parse("2006-01-02T15:04:05.999999Z", r.str("last"))
		if err == nil {
			out[r.str("job")] = t
		}
	}
	return out, nil
}

func serve(ctx context.Context, dir string, lo lakeOptions, out io.Writer) error {
	bus := relay.FromEnv()
	s := &service{dir: dir, lo: lo, out: out, pub: newPublisher(bus), api: newRelayAPI(bus)}
	defer s.pub.flush(5 * time.Second)
	if err := s.load(ctx); err != nil {
		return err
	}
	loc := s.base.location

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	windows := map[string]*window{}
	pending := map[string]*request{} // by job
	var manual []*request
	nextFire := map[string]time.Time{}
	schedules := map[string]string{}
	now := time.Now()
	last, err := s.lastRuns(ctx)
	if err != nil {
		return fmt.Errorf("reading run history: %w", err)
	}
	for _, name := range s.order {
		j := s.jobs[name]
		if j.schedule == nil {
			continue
		}
		schedules[name] = j.Schedule
		nextFire[name] = j.schedule.Next(now.In(loc))
		// A fire time missed while the service was down: one run now.
		if t, ok := last[name]; ok && !j.schedule.Next(t.In(loc)).After(now) {
			log.Printf("job %s: missed its schedule since %s; running it now", name, t.Format(time.RFC3339))
			pending[name] = &request{job: name}
		}
	}

	// Relay: read ahead of the consumer's cursor, and move the cursor only
	// past events whose runs have finished.
	batches := make(chan eventBatch)
	var filterTopic, filterType string
	setFilter := make(chan [2]string, 1)
	outstanding := map[uint64]int{}
	var readPos, acked uint64
	if s.api == nil {
		if len(s.filterJobs()) > 0 {
			log.Printf("relay is not configured (RELAY_URL or %s): jobs with on: triggers will not run on events", relay.EnvFile)
		}
	} else {
		filterTopic, filterType = s.filter()
		cursor, err := s.api.consumer(ctx, filterTopic, filterType)
		for err != nil {
			log.Printf("%v (retrying in 30s)", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(30 * time.Second):
			}
			cursor, err = s.api.consumer(ctx, filterTopic, filterType)
		}
		readPos, acked = cursor, cursor
		go s.readEvents(ctx, cursor, filterTopic, filterType, setFilter, batches)
	}
	ack := func() {
		if s.api == nil {
			return
		}
		to := readPos
		for id := range outstanding {
			to = min(to, id-1)
		}
		if to > acked {
			if err := s.api.ack(ctx, to); err != nil {
				log.Printf("%v", err)
				return
			}
			acked = to
		}
	}
	release := func(ids []uint64) {
		for _, id := range ids {
			if outstanding[id]--; outstanding[id] <= 0 {
				delete(outstanding, id)
			}
		}
	}

	log.Printf("serving %d jobs from %s", len(s.order), dir)
	done := make(chan []uint64)
	running := false
	reload := false
	failedReload := false
	for {
		// Between runs: reload, then start whatever is pending.
		if !running && reload {
			reload = false
			old := s.order
			oldJobs := s.jobs
			if err := s.load(ctx); err != nil {
				log.Printf("reload refused, keeping the running project: %v", err)
				s.pub.send(relay.Event{Topic: "lakeshape.project", Type: "failed", Data: map[string]any{"error": err.Error()}})
				failedReload = true
			} else {
				log.Printf("reloaded: %d jobs", len(s.order))
				loc = s.base.location
				if failedReload {
					s.pub.send(relay.Event{Topic: "lakeshape.project", Type: "ok"})
					failedReload = false
				}
				for _, name := range old {
					if s.jobs[name] == nil { // removed: forget its triggers
						if w := windows[name]; w != nil {
							release(w.events)
						}
						if r := pending[name]; r != nil {
							release(r.events)
						}
						delete(windows, name)
						delete(pending, name)
						delete(nextFire, name)
					}
				}
				for _, name := range s.order {
					j := s.jobs[name]
					switch {
					case j.schedule == nil:
						delete(nextFire, name)
					case oldJobs[name] == nil || schedules[name] != j.Schedule:
						nextFire[name] = j.schedule.Next(time.Now().In(loc))
					}
					schedules[name] = j.Schedule
				}
				if s.api != nil {
					t, ty := s.filter()
					if _, err := s.api.consumer(ctx, t, ty); err != nil {
						log.Printf("%v", err)
					}
					select {
					case <-setFilter:
					default:
					}
					setFilter <- [2]string{t, ty}
				}
				ack()
			}
		}
		if !running && (len(pending) > 0 || len(manual) > 0) {
			var reqs []*request
			for _, name := range s.order {
				if r := pending[name]; r != nil {
					reqs = append(reqs, r)
				}
			}
			reqs = append(reqs, manual...)
			pending, manual = map[string]*request{}, nil
			running = true
			go func() { done <- s.runAll(ctx, reqs) }()
		}

		// Sleep until the next debounce or schedule deadline.
		var wake time.Time
		for name, w := range windows {
			j := s.jobs[name]
			d := w.last.Add(j.Debounce.Duration)
			if m := w.first.Add(j.MaxWait.Duration); m.Before(d) {
				d = m
			}
			if wake.IsZero() || d.Before(wake) {
				wake = d
			}
		}
		for _, t := range nextFire {
			if wake.IsZero() || t.Before(wake) {
				wake = t
			}
		}
		var timer <-chan time.Time
		if !wake.IsZero() {
			timer = time.After(max(time.Until(wake), 0))
		}

		select {
		case <-ctx.Done():
			if running {
				<-done // the run sees ctx too: its models roll back
			}
			log.Printf("stopping")
			return nil
		case <-hup:
			log.Printf("SIGHUP: reloading after the current run")
			reload = true
		case ids := <-done:
			running = false
			release(ids)
			ack()
		case b := <-batches:
			for _, e := range b.events {
				if e.Topic == "lakeshape.trigger" {
					if e.Type != "requested" {
						continue
					}
					name, _ := e.Data["job"].(string)
					full, _ := e.Data["full_refresh"].(bool)
					if s.jobs[name] == nil {
						log.Printf("trigger: there is no job %q", name)
						s.pub.send(relay.Event{Topic: "lakeshape.run", Type: "failed", RunID: e.RunID,
							Data: map[string]any{"error": fmt.Sprintf("there is no job %q", name)}})
						continue
					}
					log.Printf("trigger: job %s requested (run %s)", name, e.RunID)
					outstanding[e.ID]++
					manual = append(manual, &request{job: name, fullRefresh: full, runID: e.RunID, events: []uint64{e.ID}})
					continue
				}
				for _, name := range s.order {
					j := s.jobs[name]
					if !slices.ContainsFunc(j.On, func(t trigger) bool { return t.matches(e) }) {
						continue
					}
					w := windows[name]
					if w == nil {
						w = &window{first: time.Now()}
						windows[name] = w
						log.Printf("job %s: triggered by %s %s (event %d)", name, e.Topic, e.Type, e.ID)
					}
					w.last = time.Now()
					w.events = append(w.events, e.ID)
					outstanding[e.ID]++
				}
			}
			readPos = max(readPos, b.next)
			ack()
		case now := <-timer:
			for name, w := range windows {
				j := s.jobs[name]
				if now.Before(w.last.Add(j.Debounce.Duration)) && now.Before(w.first.Add(j.MaxWait.Duration)) {
					continue
				}
				r := pending[name]
				if r == nil {
					r = &request{job: name}
					pending[name] = r
				}
				r.events = append(r.events, w.events...)
				delete(windows, name)
			}
			for name, t := range nextFire {
				if now.Before(t) {
					continue
				}
				if pending[name] == nil {
					pending[name] = &request{job: name}
				}
				nextFire[name] = s.jobs[name].schedule.Next(now.In(loc))
			}
		}
	}
}

func (s *service) filterJobs() []string {
	var out []string
	for _, name := range s.order {
		if len(s.jobs[name].On) > 0 {
			out = append(out, name)
		}
	}
	return out
}

// readEvents long-polls relay for events after pos and sends them on, until
// ctx ends. A new filter takes effect on the next poll.
func (s *service) readEvents(ctx context.Context, pos uint64, topic, typ string, setFilter <-chan [2]string, out chan<- eventBatch) {
	for ctx.Err() == nil {
		select {
		case f := <-setFilter:
			topic, typ = f[0], f[1]
		default:
		}
		evs, next, err := s.api.events(ctx, pos, topic, typ, "", 60*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("%v (retrying in 10s)", err)
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
				}
			}
			continue
		}
		if len(evs) == 0 && next == pos {
			continue
		}
		pos = next
		select {
		case out <- eventBatch{evs, next}:
		case <-ctx.Done():
		}
	}
}

// runAll runs the requests: each `lakeshape trigger` request alone, the
// others joined into one run per set of vars and full_refresh. It returns
// the relay events they answered.
func (s *service) runAll(ctx context.Context, reqs []*request) []uint64 {
	type group struct {
		key         string
		fullRefresh bool
		runID       string
		jobs        []string
		events      []uint64
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, r := range reqs {
		j := s.jobs[r.job]
		if j == nil { // removed by a reload
			continue
		}
		full := r.fullRefresh || j.FullRefresh
		key := varsKey(j.Vars)
		g := byKey[fmt.Sprint(full, key)]
		if r.runID != "" || g == nil {
			g = &group{key: key, fullRefresh: full, runID: r.runID}
			groups = append(groups, g)
			if r.runID == "" {
				byKey[fmt.Sprint(full, key)] = g
			}
		}
		g.jobs = append(g.jobs, r.job)
		g.events = append(g.events, r.events...)
	}

	var answered []uint64
	for _, r := range reqs {
		answered = append(answered, r.events...)
	}
	for _, g := range groups {
		if ctx.Err() != nil {
			break
		}
		e := s.envs[g.key]
		picked := map[string]bool{}
		for _, name := range g.jobs {
			j := s.jobs[name]
			ms, err := e.p.selectModels(j.Select, j.Exclude)
			if err != nil { // checked at load; the project can't have changed
				log.Printf("job %s: %v", name, err)
				continue
			}
			for _, m := range ms {
				picked[m.name] = true
			}
		}
		var models []*model
		for _, m := range e.p.order {
			if picked[m.name] {
				models = append(models, m)
			}
		}
		runID := g.runID
		if runID == "" {
			runID = newRunID()
		}
		_, err := e.l.build(ctx, models, runOptions{runID: runID, jobs: g.jobs, fullRefresh: g.fullRefresh,
			out: s.out, notify: s.pub.notify(runID)})
		if err != nil {
			log.Printf("run %s (%s): %v", runID, strings.Join(g.jobs, ", "), err)
			s.pub.send(relay.Event{Topic: "lakeshape.run", Type: "failed", RunID: runID,
				Data: map[string]any{"jobs": g.jobs, "error": err.Error()}})
		}
	}
	return answered
}

// triggerJob asks the running service to run a job now, through relay, and
// follows the run until it ends.
func triggerJob(ctx context.Context, p *project, name string, fullRefresh bool, out io.Writer) error {
	if !slices.ContainsFunc(p.jobs, func(j *job) bool { return j.Name == name }) {
		return fmt.Errorf("there is no job %q in jobs.yml", name)
	}
	bus := relay.FromEnv()
	if bus == nil {
		return fmt.Errorf("relay is not configured (RELAY_URL or %s): trigger asks the service through relay", relay.EnvFile)
	}
	api := newRelayAPI(bus)
	runID := newRunID()
	ev, err := bus.Publish(ctx, relay.Event{Topic: "lakeshape.trigger", Type: "requested", RunID: runID,
		Data: map[string]any{"job": name, "full_refresh": fullRefresh}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "asked the service to run job %s (run %s); waiting for it\n", name, runID)
	pos := ev.ID
	heard := false
	for {
		evs, next, err := api.events(ctx, pos, "lakeshape.run,lakeshape.model", "", runID, 60*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		pos = next
		if len(evs) == 0 && !heard {
			fmt.Fprintln(out, "  no word from the service yet: is it running? (systemctl status lakeshape) Still waiting; Ctrl-C stops waiting, not the run")
		}
		heard = heard || len(evs) > 0
		for _, e := range evs {
			switch {
			case e.Topic == "lakeshape.model":
				info, _ := e.Data["error"].(string)
				fmt.Fprintf(out, "  %-8s %s %s\n", e.Type, e.Data["relation"], firstLine(info))
			case e.Type == "ok":
				fmt.Fprintln(out, "done: ok")
				return nil
			case e.Type == "failed":
				msg, _ := e.Data["error"].(string)
				return &exitError{1, fmt.Errorf("run %s failed: %s", runID, msg)}
			}
		}
	}
}
