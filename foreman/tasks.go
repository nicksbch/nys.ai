package main

// The engine turns triggers into tasks and tasks into runs: relay events
// (one consumer per task type), schedules, and requests from you. It groups
// repeated events, keeps to the concurrency limits, retries runs that could
// not start, and publishes what happens to relay.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	relay "relay/client"
)

const maxEventText = 4 << 10 // longest result field in a relay event

// Variables so tests can shorten them.
var (
	retryFirst = 5 * time.Second // first wait before retrying a run that couldn't start
	retryMax   = 5 * time.Minute
	pollWait   = "30s" // relay long-poll
)

type engine struct {
	defs    *Defs
	store   *store
	bus     *relay.Client // nil: relay not configured
	hubURL  string
	hubTok  string
	slots   chan struct{}          // overall concurrency
	agentMu map[string]*sync.Mutex // one run per agent at a time
	wg      sync.WaitGroup

	mu     sync.Mutex
	active map[string]chan struct{} // tasks being run, closed when done

	actMu sync.Mutex // one decision on an action at a time
}

func newEngine(defs *Defs, st *store, bus *relay.Client, hubURL, hubToken string, concurrency int) *engine {
	e := &engine{defs: defs, store: st, bus: bus, hubURL: hubURL, hubTok: hubToken,
		slots: make(chan struct{}, concurrency), agentMu: map[string]*sync.Mutex{}, active: map[string]chan struct{}{}}
	for name := range defs.Agents {
		e.agentMu[name] = &sync.Mutex{}
	}
	return e
}

// start resumes interrupted work, then starts the triggers. Runs of tasks
// that were created or running when foreman stopped are started again.
func (e *engine) start(ctx context.Context) {
	for _, t := range e.store.find(func(t *Task) bool { return t.Status == statusCreated || t.Status == statusRunning }) {
		if t.Status == statusRunning {
			for _, r := range t.Runs {
				if r.Status == "running" {
					e.endRun(t, r, &runResult{Status: "interrupted", Error: "foreman stopped during the run"}, time.Now())
				}
			}
		}
		if e.defs.Types[t.Type] == nil {
			log.Printf("task %s: task type %q no longer exists; not resuming it", t.ID, t.Type)
			continue
		}
		log.Printf("task %s: resuming", t.ID)
		e.goRun(ctx, t.ID, "", "")
	}
	e.resumeActions(ctx)
	for _, tt := range e.defs.Types {
		if tt.OnTopic != "" {
			if e.bus == nil {
				log.Printf("task type %s: relay is not configured (RELAY_URL), so on_event can't trigger", tt.Name)
			} else {
				e.wg.Add(1)
				go func() { defer e.wg.Done(); e.consume(ctx, tt) }()
			}
		}
		if tt.Every > 0 {
			e.wg.Add(1)
			go func() { defer e.wg.Done(); e.schedule(ctx, tt) }()
		}
	}
}

// wait blocks until every trigger and run has stopped.
func (e *engine) wait() { e.wg.Wait() }

// --- creating tasks -----------------------------------------------------------

func newTaskID() string { return newRunID() }

func (e *engine) create(tt *TaskType, origin, parent, title, key, input string, eventID uint64) (*Task, error) {
	now := time.Now().UTC()
	t := &Task{ID: newTaskID(), Type: tt.Name, Title: title, Key: key, Origin: origin, Parent: parent, EventID: eventID,
		Input: input, Status: statusCreated, Created: now, Updated: now, Seen: 1, LastSeen: now, Runs: []*RunInfo{}}
	if eventID != 0 {
		t.Events = []uint64{eventID}
	}
	if err := e.store.write(logRecord{T: now, Op: "created", Task: t.ID, New: t}); err != nil {
		return nil, err
	}
	e.publish(tt.Name, relay.Event{Type: "created", Cause: eventID, Data: e.eventData(t, nil, nil)})
	log.Printf("task %s (%s): created: %s", t.ID, tt.Name, title)
	return e.store.get(t.ID), nil
}

// createUserTask is a task you start, with your text as its input.
func (e *engine) createUserTask(ctx context.Context, typeName, input string) (*Task, error) {
	tt := e.defs.Types[typeName]
	if tt == nil {
		return nil, fmt.Errorf("no task type %q", typeName)
	}
	if strings.TrimSpace(input) == "" {
		return nil, fmt.Errorf("input is empty")
	}
	t, err := e.create(tt, "user", "", title(tt, firstLine(input, 80)), "", input, 0)
	if err != nil {
		return nil, err
	}
	e.goRun(ctx, t.ID, "", "")
	return t, nil
}

func title(tt *TaskType, detail string) string {
	base := tt.Description
	if base == "" {
		base = tt.Name
	}
	if detail == "" {
		return base
	}
	return base + ": " + detail
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// --- relay events -------------------------------------------------------------

type relayEvent struct {
	ID    uint64         `json:"id"`
	Time  time.Time      `json:"time"`
	Topic string         `json:"topic"`
	Type  string         `json:"type"`
	RunID string         `json:"run_id,omitempty"`
	Cause uint64         `json:"cause,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// consume reads a task type's events from its own relay consumer, one at a
// time. An event is acked only once it is handled: grouped into an open
// task, or its new task's run has ended. If the run can't start, it is
// retried, and the event stays unacked meanwhile.
func (e *engine) consume(ctx context.Context, tt *TaskType) {
	name := "foreman-" + tt.Name
	backoff := time.Second
	retry := func(err error) {
		log.Printf("task type %s: relay: %v (retrying in %s)", tt.Name, err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	for ctx.Err() == nil {
		err := e.relayDo(ctx, http.MethodPut, "/v1/consumers/"+name,
			map[string]string{"topic": tt.OnTopic, "type": strings.Join(tt.OnTypes, ",")}, nil)
		if err == nil {
			break
		}
		retry(err)
	}
	log.Printf("task type %s: reading %s %s from relay as consumer %q", tt.Name, tt.OnTopic, strings.Join(tt.OnTypes, ","), name)
	for ctx.Err() == nil {
		var resp struct {
			Events []relayEvent `json:"events"`
			Gap    bool         `json:"gap"`
		}
		if err := e.relayDo(ctx, http.MethodGet, "/v1/consumers/"+name+"/next?limit=1&wait="+pollWait, nil, &resp); err != nil {
			if ctx.Err() == nil {
				retry(err)
			}
			continue
		}
		if resp.Gap {
			log.Printf("task type %s: some events expired in relay before foreman read them", tt.Name)
		}
		if len(resp.Events) == 0 {
			continue
		}
		ev := resp.Events[0]
		if !e.handleEvent(ctx, tt, ev) {
			return // stopping
		}
		for ctx.Err() == nil {
			err := e.relayDo(ctx, http.MethodPost, "/v1/consumers/"+name+"/ack", map[string]uint64{"id": ev.ID}, nil)
			if err == nil {
				break
			}
			retry(err)
		}
		backoff = time.Second
	}
}

// handleEvent groups the event into an open task, or creates a task and
// runs it to the end. It returns false if foreman is stopping first.
func (e *engine) handleEvent(ctx context.Context, tt *TaskType, ev relayEvent) bool {
	// An event seen before (redelivered after a restart) only needs its task
	// to have finished.
	if t := e.taskWithEvent(ev.ID); t != nil {
		log.Printf("task type %s: event %d is already in task %s", tt.Name, ev.ID, t.ID)
		return e.waitRunnable(ctx, t.ID)
	}
	// An event from the job of an action a task sent: into that task if it is
	// of this type, open or closed, so an action can't start a loop of runs.
	origin, parent := "event", ""
	if p := e.taskOfJob(ev); p != nil {
		if p.Type == tt.Name {
			if err := e.store.write(logRecord{Op: "attached", Task: p.ID, Event: ev.ID}); err != nil {
				log.Printf("task %s: %v", p.ID, err)
			}
			log.Printf("task %s (%s): event %d came from its action's job; attached to it", p.ID, tt.Name, ev.ID)
			return true
		}
		origin, parent = "agent", p.ID
	}
	key := groupKey(tt, ev)
	if key != "" {
		open := e.store.find(func(t *Task) bool { return t.Type == tt.Name && t.Key == key && !closed(t.Status) })
		if len(open) > 0 {
			t := open[0]
			if err := e.store.write(logRecord{Op: "attached", Task: t.ID, Event: ev.ID}); err != nil {
				log.Printf("task %s: %v", t.ID, err)
			}
			log.Printf("task %s (%s): event %d grouped into it (seen %d times)", t.ID, tt.Name, ev.ID, t.Seen+1)
			return true
		}
	}
	b, _ := json.MarshalIndent(ev, "", "  ")
	detail, _ := ev.Data["name"].(string)
	if detail == "" {
		detail = ev.Topic + " " + ev.Type
	}
	t, err := e.create(tt, origin, parent, title(tt, firstLine(detail, 120)), key, "Event:\n"+string(b), ev.ID)
	if err != nil {
		log.Printf("task type %s: could not create a task for event %d: %v", tt.Name, ev.ID, err)
		return e.sleep(ctx, retryFirst) // the event stays unacked and comes back
	}
	return e.runToEnd(ctx, t.ID, "", "")
}

func (e *engine) taskWithEvent(id uint64) *Task {
	found := e.store.find(func(t *Task) bool { return slices.Contains(t.Events, id) })
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

// waitRunnable runs a task that is still waiting for its run (it was
// interrupted), and returns once it is no longer waiting.
func (e *engine) waitRunnable(ctx context.Context, id string) bool {
	t := e.store.get(id)
	if t.Status == statusCreated {
		return e.runToEnd(ctx, id, "", "")
	}
	return ctx.Err() == nil
}

// groupKey is the task type's name and the group_by fields of the event, or
// "" when the type doesn't group.
func groupKey(tt *TaskType, ev relayEvent) string {
	if len(tt.GroupBy) == 0 {
		return ""
	}
	parts := []string{tt.Name}
	for _, k := range tt.GroupBy {
		var v any
		switch k {
		case "topic":
			v = ev.Topic
		case "type":
			v = ev.Type
		case "run_id":
			v = ev.RunID
		default:
			v = ev.Data[strings.TrimPrefix(k, "data.")]
		}
		if v == nil {
			v = ""
		}
		parts = append(parts, fmt.Sprint(v))
	}
	return strings.Join(parts, "|")
}

func (e *engine) relayDo(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.bus.URL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.bus.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.bus.Token)
	}
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- schedules ----------------------------------------------------------------

// schedule creates a task on every tick, unless the type's previous task is
// still waiting or running.
func (e *engine) schedule(ctx context.Context, tt *TaskType) {
	tick := time.NewTicker(tt.Every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			prev := e.store.find(func(t *Task) bool { return t.Type == tt.Name && t.Origin == "schedule" })
			if len(prev) > 0 && (prev[0].Status == statusCreated || prev[0].Status == statusRunning) {
				log.Printf("task type %s: skipping this tick: task %s is still %s", tt.Name, prev[0].ID, prev[0].Status)
				continue
			}
			input := fmt.Sprintf("Scheduled run at %s.", now.UTC().Format(time.RFC3339))
			if len(prev) > 0 {
				input += fmt.Sprintf(" Previous scheduled task: %s, last updated %s.", prev[0].Status, prev[0].Updated.Format(time.RFC3339))
			} else {
				input += " This is the first scheduled task of this type."
			}
			t, err := e.create(tt, "schedule", "", title(tt, now.UTC().Format("2006-01-02 15:04")), "", input, 0)
			if err != nil {
				log.Printf("task type %s: %v", tt.Name, err)
				continue
			}
			e.goRun(ctx, t.ID, "", "")
		}
	}
}

// --- runs ---------------------------------------------------------------------

// rerun starts a new run of a finished task, optionally with another agent
// or model.
func (e *engine) rerun(ctx context.Context, id, agent, model string) error {
	t := e.store.get(id)
	switch {
	case t == nil:
		return fmt.Errorf("no task %q", id)
	case t.Status != statusAnswered && t.Status != statusFailed:
		return fmt.Errorf("task is %s: only answered or failed tasks can be run again", t.Status)
	case agent != "" && e.defs.Agents[agent] == nil:
		return fmt.Errorf("no agent %q", agent)
	case model != "" && e.defs.Models[model] == nil:
		return fmt.Errorf("no model %q", model)
	}
	e.goRun(ctx, id, agent, model)
	return nil
}

func (e *engine) goRun(ctx context.Context, id, agent, model string) {
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.runToEnd(ctx, id, agent, model) }()
}

// runToEnd runs a task once to its end. A run that can't take place (model
// or hub unavailable) is retried, waiting 5 s and doubling up to 5 min, and
// the task shows why it is waiting. It returns false if foreman is stopping.
func (e *engine) runToEnd(ctx context.Context, id, agentName, modelName string) bool {
	// Only one runner per task: a second caller waits for the first.
	e.mu.Lock()
	if ch, ok := e.active[id]; ok {
		e.mu.Unlock()
		select {
		case <-ch:
			return true
		case <-ctx.Done():
			return false
		}
	}
	ch := make(chan struct{})
	e.active[id] = ch
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.active, id)
		e.mu.Unlock()
		close(ch)
	}()

	wait := retryFirst
	for {
		err := e.runOnce(ctx, id, agentName, modelName)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		log.Printf("task %s: %v (retrying in %s)", id, err, wait)
		e.store.write(logRecord{Op: "waiting", Task: id, Text: fmt.Sprintf("%v; retrying in %s", err, wait)})
		if !e.sleep(ctx, wait) {
			return false
		}
		wait = min(wait*2, retryMax)
	}
}

func (e *engine) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// runOnce waits for its turn, then runs the task. It returns an error only
// when the run couldn't take place.
func (e *engine) runOnce(ctx context.Context, id, agentName, modelName string) error {
	t := e.store.get(id)
	tt := e.defs.Types[t.Type]
	if tt == nil {
		return fmt.Errorf("task type %q no longer exists", t.Type)
	}
	if agentName == "" {
		agentName = tt.Agent
	}
	agent := e.defs.Agents[agentName]
	if modelName == "" {
		modelName = agent.Model
	}
	model := e.defs.Models[modelName]

	mu := e.agentMu[agent.Name]
	mu.Lock()
	defer mu.Unlock()
	select {
	case e.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.slots }()

	started := time.Now().UTC()
	info := &RunInfo{ID: newRunID(), Agent: agent.Name, Model: modelName, Started: started, Status: "running"}
	rec, done, err := e.store.transcript(info)
	if err != nil {
		return err
	}
	defer done()
	if err := e.store.write(logRecord{Op: "run_started", Task: id, Run: info}); err != nil {
		return err
	}
	e.publish(tt.Name, relay.Event{Type: "started", RunID: info.ID, Data: e.eventData(t, info, nil)})
	log.Printf("task %s (%s): run %s started (%s, %s)", id, tt.Name, info.ID, agent.Name, modelName)

	res, err := run(ctx, runSpec{RunID: info.ID, Agent: agent, Type: tt, Model: model, ModelKey: modelName,
		Input: t.Input, HubURL: e.hubURL, HubToken: e.hubTok}, rec)
	if err != nil {
		// The run couldn't take place, or not to the end: it is kept, marked
		// so, and the task goes back to waiting for a run.
		res = &runResult{Status: "unavailable", Error: err.Error()}
		if ctx.Err() != nil {
			res = &runResult{Status: "interrupted", Error: "foreman stopped during the run"}
		}
		e.endRun(e.store.get(id), info, res, time.Now())
		return err
	}
	e.endRun(e.store.get(id), info, res, time.Now())
	return nil
}

func (e *engine) endRun(t *Task, info *RunInfo, res *runResult, end time.Time) {
	ended := *info
	ended.Ended, ended.Status, ended.Error = end.UTC(), res.Status, res.Error
	ended.Steps, ended.Usage, ended.Duration = res.Steps, res.Usage, end.Sub(info.Started).Milliseconds()
	// A proposal counts only from a run that finished its work; otherwise it
	// stays in the transcript alone.
	var action *proposal
	if res.Status == "ok" {
		action = res.Action
	}
	if err := e.store.write(logRecord{Op: "run_ended", Task: t.ID, Run: &ended, Result: res.Result, Action: action}); err != nil {
		log.Printf("task %s: %v", t.ID, err)
	}
	// Only real outcomes go to relay: vigil counts "failed" as a failed job,
	// and a model being down is not the agent failing.
	if res.Status == "ok" || res.Status == "failed" {
		e.publish(t.Type, relay.Event{Type: res.Status, RunID: info.ID, Data: e.eventData(t, &ended, res)})
	}
	if action != nil {
		e.publish(t.Type, relay.Event{Type: "action_proposed", RunID: info.ID, Data: e.actionData(t, action.Tool, "", "")})
		log.Printf("task %s (%s): run %s proposed %s", t.ID, t.Type, info.ID, action.Tool)
	}
	log.Printf("task %s (%s): run %s %s after %d steps%s", t.ID, t.Type, info.ID, res.Status, res.Steps, errSuffix(res.Error))
}

func errSuffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// --- your actions -------------------------------------------------------------

func (e *engine) setStatus(id, status string) error {
	t := e.store.get(id)
	switch {
	case t == nil:
		return fmt.Errorf("no task %q", id)
	case status != statusResolved && status != statusDismissed:
		return fmt.Errorf(`status must be "resolved" or "dismissed"`)
	case t.Status == statusRunning:
		return fmt.Errorf("the task is running; wait for the run to end")
	case closed(t.Status):
		return fmt.Errorf("the task is already %s", t.Status)
	}
	if err := e.store.write(logRecord{Op: "status", Task: id, Status: status}); err != nil {
		return err
	}
	e.publish(t.Type, relay.Event{Type: status, Data: e.eventData(t, nil, nil)})
	return nil
}

func (e *engine) setVerdict(id, verdict, note string) error {
	t := e.store.get(id)
	switch {
	case t == nil:
		return fmt.Errorf("no task %q", id)
	case verdict != "good" && verdict != "bad" && verdict != "":
		return fmt.Errorf(`verdict must be "good", "bad", or empty to clear it`)
	}
	return e.store.write(logRecord{Op: "verdict", Task: id, Value: verdict, Text: note})
}

// --- relay events out ---------------------------------------------------------

func (e *engine) publish(typeName string, ev relay.Event) {
	ev.Topic = "foreman." + typeName
	e.bus.Go(ev) // a nil bus is a no-op
}

func (e *engine) eventData(t *Task, run *RunInfo, res *runResult) map[string]any {
	d := map[string]any{"task_id": t.ID, "name": t.Title, "origin": t.Origin}
	if run != nil {
		d["agent"], d["model"] = run.Agent, run.Model
		if !run.Ended.IsZero() {
			d["steps"], d["duration_ms"] = run.Steps, run.Duration
			d["input_tokens"], d["output_tokens"] = run.Usage.In, run.Usage.Out
		}
	}
	if res != nil {
		if res.Error != "" {
			d["error"] = res.Error
		}
		if len(res.Result) > 0 {
			result := map[string]string{}
			for k, v := range res.Result {
				if len(v) > maxEventText {
					v = v[:maxEventText] + "…"
				}
				result[k] = v
			}
			d["result"] = result
		}
	}
	return d
}
