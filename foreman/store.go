package main

// Tasks and transcripts on disk. tasks.jsonl is an append-only log of what
// happened to each task; the current state of every task is rebuilt from it
// at startup. Each run's transcript is its own JSON-lines file.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Task statuses.
const (
	statusCreated   = "created"  // waiting for its first run
	statusRunning   = "running"  // an agent is working on it
	statusAnswered  = "answered" // the agent submitted a result
	statusFailed    = "failed"   // the last run ended without a result
	statusResolved  = "resolved" // you closed it: fixed
	statusDismissed = "dismissed"
)

func closed(status string) bool { return status == statusResolved || status == statusDismissed }

// Action states.
const (
	actionProposed   = "proposed"   // waiting for you
	actionCalling    = "calling"    // you approved; the tool is being called
	actionWaiting    = "waiting"    // a long tool started a job; followed on relay
	actionDone       = "done"       // the call or its job succeeded
	actionFailed     = "failed"     // the call or its job failed; can be approved again
	actionNoReply    = "no_reply"   // the job didn't end within its wait
	actionRejected   = "rejected"   // you rejected it
	actionSuperseded = "superseded" // a later run proposed another one
)

// Action is a call to a tool that changes something, proposed by a run and
// made only after you approve it.
type Action struct {
	Run      string          `json:"run"`   // the run that proposed it, which also identifies it
	Agent    string          `json:"agent"` // the call goes through this agent's hub profile
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
	Preview  string          `json:"preview,omitempty"`
	WaitMS   int64           `json:"wait_ms"`
	State    string          `json:"state"`
	Note     string          `json:"note,omitempty"` // yours, with the decision
	Attempts int             `json:"attempts,omitempty"`
	Result   string          `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	JobRunID string          `json:"job_run_id,omitempty"` // a long tool's job on relay
	JobTopic string          `json:"job_topic,omitempty"`
	Proposed time.Time       `json:"proposed"`
	Called   time.Time       `json:"called,omitzero"`
	Ended    time.Time       `json:"ended,omitzero"`
}

type Task struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Title    string            `json:"title"`
	Key      string            `json:"key,omitempty"`      // grouping key
	Origin   string            `json:"origin"`             // event, schedule, user, agent (an event an action caused)
	Parent   string            `json:"parent,omitempty"`   // origin agent: the task whose action caused it
	EventID  uint64            `json:"event_id,omitempty"` // the relay event that created it
	Input    string            `json:"input"`              // the first user message of every run
	Status   string            `json:"status"`
	Created  time.Time         `json:"created"`
	Updated  time.Time         `json:"updated"`
	Seen     int               `json:"seen"` // events grouped into it, the first included
	LastSeen time.Time         `json:"last_seen"`
	Events   []uint64          `json:"events,omitempty"`  // relay ids of those events
	Result   map[string]string `json:"result,omitempty"`  // from the last run that submitted one
	Waiting  string            `json:"waiting,omitempty"` // why a run couldn't start, while it is retried
	Verdict  string            `json:"verdict,omitempty"`
	Note     string            `json:"note,omitempty"`
	Runs     []*RunInfo        `json:"runs"`
	Actions  []*Action         `json:"actions,omitempty"` // oldest first; the last is the current one
}

// action is the task's current action, or nil.
func (t *Task) action() *Action {
	if len(t.Actions) == 0 {
		return nil
	}
	return t.Actions[len(t.Actions)-1]
}

func (t *Task) actionOf(run string) *Action {
	for _, a := range t.Actions {
		if a.Run == run {
			return a
		}
	}
	return nil
}

type RunInfo struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Model    string    `json:"model"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	Status   string    `json:"status"` // running, ok, failed, unavailable (couldn't reach the model or hub), interrupted
	Error    string    `json:"error,omitempty"`
	Steps    int       `json:"steps"`
	Usage    usage     `json:"usage"`
	Duration int64     `json:"duration_ms,omitempty"`
}

// logRecord is one line of tasks.jsonl.
type logRecord struct {
	T         time.Time         `json:"t"`
	Op        string            `json:"op"` // created, attached, run_started, run_ended, status, verdict, waiting, action_*
	Task      string            `json:"task"`
	New       *Task             `json:"new,omitempty"`        // created
	Event     uint64            `json:"event,omitempty"`      // attached
	Run       *RunInfo          `json:"run,omitempty"`        // run_started, run_ended
	Result    map[string]string `json:"result,omitempty"`     // run_ended
	Status    string            `json:"status,omitempty"`     // status
	Text      string            `json:"text,omitempty"`       // verdict note, waiting reason
	Value     string            `json:"value,omitempty"`      // verdict, action decision, action end state
	Action    *proposal         `json:"action,omitempty"`     // run_ended: the run's proposal
	ActionRun string            `json:"action_run,omitempty"` // action_*: the action's run
	Output    string            `json:"output,omitempty"`     // action_ended: the result
	JobRun    string            `json:"job_run,omitempty"`    // action_called: a long tool's job
	JobTopic  string            `json:"job_topic,omitempty"`
}

type store struct {
	dir string

	mu    sync.Mutex
	log   *os.File
	tasks map[string]*Task
}

func openStore(dir string) (*store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o755); err != nil {
		return nil, err
	}
	s := &store{dir: dir, tasks: map[string]*Task{}}
	path := filepath.Join(dir, "tasks.jsonl")
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for n := 1; sc.Scan(); n++ {
			var r logRecord
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				log.Printf("tasks.jsonl line %d: skipping unreadable record: %v", n, err) // e.g. cut off by a crash
				continue
			}
			s.apply(r)
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.log = f
	return s, nil
}

// apply updates the in-memory state with one record.
func (s *store) apply(r logRecord) {
	if r.Op == "created" {
		if r.New != nil {
			t := *r.New
			s.tasks[t.ID] = &t
		}
		return
	}
	t := s.tasks[r.Task]
	if t == nil {
		return
	}
	t.Updated = r.T
	switch r.Op {
	case "attached":
		t.Seen++
		t.LastSeen = r.T
		t.Events = append(t.Events, r.Event)
	case "waiting":
		t.Waiting = r.Text
	case "run_started":
		run := *r.Run
		t.Runs = append(t.Runs, &run)
		t.Status, t.Waiting = statusRunning, ""
	case "run_ended":
		for i, run := range t.Runs {
			if run.ID == r.Run.ID {
				ended := *r.Run
				t.Runs[i] = &ended
			}
		}
		switch r.Run.Status {
		case "ok":
			t.Status, t.Result = statusAnswered, r.Result
			if p := r.Action; p != nil {
				for _, a := range t.Actions {
					if a.State == actionProposed || a.State == actionFailed || a.State == actionNoReply {
						a.State = actionSuperseded
					}
				}
				t.Actions = append(t.Actions, &Action{Run: r.Run.ID, Agent: r.Run.Agent, Tool: p.Tool, Args: p.Args,
					Preview: p.Preview, WaitMS: p.WaitMS, State: actionProposed, Proposed: r.T})
			}
		case "failed":
			t.Status = statusFailed
		default: // unavailable or interrupted: the task waits for another run
			t.Status = statusCreated
		}
	case "status":
		t.Status = r.Status
	case "verdict":
		t.Verdict, t.Note = r.Value, r.Text
	case "action_decided", "action_called", "action_ended":
		a := t.actionOf(r.ActionRun)
		if a == nil {
			return
		}
		switch r.Op {
		case "action_decided":
			a.Note = r.Text
			if a.State = actionRejected; r.Value == "approve" {
				a.State, a.Attempts, a.Result, a.Error = actionCalling, a.Attempts+1, "", ""
				a.JobRunID, a.JobTopic, a.Called, a.Ended = "", "", time.Time{}, time.Time{}
			}
		case "action_called":
			a.State, a.JobRunID, a.JobTopic, a.Called = actionWaiting, r.JobRun, r.JobTopic, r.T
		case "action_ended":
			a.State, a.Result, a.Error, a.Ended = r.Value, r.Output, r.Text, r.T
			if a.Called.IsZero() {
				a.Called = r.T
			}
		}
	}
}

// write appends a record, fsyncs it and applies it.
func (s *store) write(r logRecord) error {
	if r.T.IsZero() {
		r.T = time.Now().UTC()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.log.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := s.log.Sync(); err != nil {
		return err
	}
	s.apply(r)
	return nil
}

// get returns a copy of a task, safe to read without the lock.
func (s *store) get(id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyTask(s.tasks[id])
}

func copyTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	c := *t
	c.Events = append([]uint64(nil), t.Events...)
	c.Runs = make([]*RunInfo, len(t.Runs))
	for i, r := range t.Runs {
		rc := *r
		c.Runs[i] = &rc
	}
	if t.Actions != nil {
		c.Actions = make([]*Action, len(t.Actions))
		for i, a := range t.Actions {
			ac := *a
			c.Actions[i] = &ac
		}
	}
	return &c
}

// find returns copies of the tasks for which keep is true, newest first.
func (s *store) find(keep func(*Task) bool) []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Task
	for _, t := range s.tasks {
		if keep(t) {
			out = append(out, copyTask(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// --- transcripts --------------------------------------------------------------

func (s *store) runPath(run *RunInfo) string {
	return filepath.Join(s.dir, "runs", run.Started.UTC().Format("2006-01-02"), run.ID+".jsonl")
}

// transcript opens a run's file and returns a recorder that appends to it.
// Each record is fsynced, so a crash loses at most the step in progress.
func (s *store) transcript(run *RunInfo) (recorder, func(), error) {
	p := s.runPath(run)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	var mu sync.Mutex
	rec := func(kind string, data map[string]any) {
		data["kind"], data["t"] = kind, time.Now().UTC()
		b, err := json.Marshal(data)
		if err != nil {
			log.Printf("run %s: %v", run.ID, err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		f.Write(append(b, '\n'))
		f.Sync()
	}
	return rec, func() { f.Close() }, nil
}

// readTranscript returns a run's records in order.
func (s *store) readTranscript(run *RunInfo) ([]json.RawMessage, error) {
	f, err := os.Open(s.runPath(run))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if json.Valid(sc.Bytes()) {
			out = append(out, append(json.RawMessage(nil), sc.Bytes()...))
		}
	}
	return out, sc.Err()
}

// --- retention ----------------------------------------------------------------

// expire drops closed tasks last updated before cutoff, with their
// transcripts, and rewrites tasks.jsonl without them. Open tasks are kept
// however old.
func (s *store) expire(cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var drop []*Task
	for _, t := range s.tasks {
		if closed(t.Status) && t.Updated.Before(cutoff) {
			drop = append(drop, t)
		}
	}
	if len(drop) == 0 {
		return 0, nil
	}
	for _, t := range drop {
		for _, r := range t.Runs {
			os.Remove(s.runPath(r))
		}
		delete(s.tasks, t.ID)
	}
	// Rewrite the log as one "created" record per kept task, holding its
	// current state, then swap it in.
	path := filepath.Join(s.dir, "tasks.jsonl")
	tmp, err := os.CreateTemp(s.dir, "tasks-*.jsonl")
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(tmp)
	ids := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return s.tasks[ids[i]].Created.Before(s.tasks[ids[j]].Created) })
	for _, id := range ids {
		b, _ := json.Marshal(logRecord{T: s.tasks[id].Created, Op: "created", Task: id, New: s.tasks[id]})
		w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err == nil {
		err = tmp.Sync()
	}
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	s.log.Close()
	if s.log, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
		return 0, err
	}
	// Remove day directories left empty.
	days, _ := filepath.Glob(filepath.Join(s.dir, "runs", "*"))
	for _, d := range days {
		os.Remove(d) // fails, harmlessly, when not empty
	}
	return len(drop), nil
}
