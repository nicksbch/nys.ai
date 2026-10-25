package main

// Actions: a run proposes a call to a tool that changes something, and the
// call is made only when you approve it. The tool belongs to the service that
// does the work and is reached through mcp-hub. A long tool returns the
// run_id and topic of the job it started, and its ok or failed is read from
// relay. See SPEC-actions.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	mcphub "mcp-hub/client"
	relay "relay/client"
)

const jobsConsumer = "foreman-actions"

// Variables so tests can shorten them.
var (
	actionCallTimeout = 70 * time.Second // a little over the hub's 60 s, so its own timeout message comes back
	deadlineEvery     = 30 * time.Second // how often waiting actions are checked against their wait
)

// resumeActions settles what a restart left behind, then follows long jobs
// on relay and their deadlines, if any task type has actions.
func (e *engine) resumeActions(ctx context.Context) {
	for _, t := range e.store.find(func(t *Task) bool { return len(t.Actions) > 0 }) {
		for _, a := range t.Actions {
			if a.State == actionCalling {
				// Not called again by itself: it may have happened.
				e.endAction(t, a, actionFailed, "", "interrupted: foreman stopped during the call, which may have happened")
			}
		}
	}
	if !slices.ContainsFunc(mapValues(e.defs.Types), func(tt *TaskType) bool { return len(tt.Actions) > 0 }) {
		return
	}
	if e.bus == nil {
		log.Print("relay is not configured (RELAY_URL), so the jobs of long actions can't be followed")
	} else {
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.followJobs(ctx) }()
	}
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.watchDeadlines(ctx) }()
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// decideAction approves or rejects a task's current action. Approving starts
// the call in the background; the state moves to calling first, under the
// lock, so two clicks call once.
func (e *engine) decideAction(ctx context.Context, id, decision, note string) error {
	e.actMu.Lock()
	defer e.actMu.Unlock()
	t := e.store.get(id)
	if t == nil {
		return fmt.Errorf("no task %q", id)
	}
	a := t.action()
	switch {
	case decision != "approve" && decision != "reject":
		return fmt.Errorf(`decision must be "approve" or "reject"`)
	case a == nil:
		return fmt.Errorf("the task has no action")
	case closed(t.Status):
		return fmt.Errorf("the task is %s", t.Status)
	case decision == "reject" && a.State != actionProposed:
		return fmt.Errorf("the action is %s: only a proposed action can be rejected", a.State)
	case decision == "approve" && a.State != actionProposed && a.State != actionFailed && a.State != actionNoReply:
		return fmt.Errorf("the action is %s: only a proposed, failed or no_reply action can be approved", a.State)
	}
	if err := e.store.write(logRecord{Op: "action_decided", Task: id, ActionRun: a.Run, Value: decision, Text: note}); err != nil {
		return err
	}
	if decision == "reject" {
		e.publish(t.Type, relay.Event{Type: "action_rejected", RunID: a.Run, Data: e.actionData(t, a.Tool, "", note)})
		log.Printf("task %s: action %s rejected", id, a.Tool)
		return nil
	}
	log.Printf("task %s: action %s approved; calling it", id, a.Tool)
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.callAction(ctx, id, a.Run) }()
	return nil
}

// callAction makes the approved call through the hub, with the proposing
// run's id, and records what came back.
func (e *engine) callAction(ctx context.Context, id, run string) {
	t := e.store.get(id)
	a := t.actionOf(run)
	agent := e.defs.Agents[a.Agent]
	if agent == nil {
		e.endAction(t, a, actionFailed, "", fmt.Sprintf("agent %q no longer exists, so its hub profile is unknown", a.Agent))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, actionCallTimeout)
	defer cancel()
	hub, err := connectHub(cctx, runSpec{RunID: a.Run, Agent: agent, HubURL: e.hubURL, HubToken: e.hubTok})
	if err != nil {
		e.endAction(t, a, actionFailed, "", "mcp-hub: "+err.Error())
		return
	}
	defer hub.Close()
	var args map[string]any
	json.Unmarshal(a.Args, &args)
	res, err := hub.CallTool(cctx, &mcp.CallToolParams{Name: a.Tool, Arguments: args})
	switch {
	case ctx.Err() != nil:
		return // stopping: left calling, and marked interrupted at the next start
	case err != nil:
		e.endAction(t, a, actionFailed, "", err.Error())
	case res.IsError:
		e.endAction(t, a, actionFailed, "", mcphub.ResultText(res))
	default:
		runID, topic := mcphub.JobOf(res)
		if runID == "" {
			e.endAction(t, a, actionDone, mcphub.ResultText(res), "")
			return
		}
		if err := e.store.write(logRecord{Op: "action_called", Task: id, ActionRun: a.Run, JobRun: runID, JobTopic: topic}); err != nil {
			log.Printf("task %s: %v", id, err)
		}
		log.Printf("task %s: action %s started job %s on %s", id, a.Tool, runID, topic)
		e.lookBack(ctx, runID, topic)
	}
}

// lookBack matches a job that ended before its action was recorded as
// waiting, which the consumer would have passed over.
func (e *engine) lookBack(ctx context.Context, runID, topic string) {
	if e.bus == nil {
		return
	}
	var resp struct {
		Events []relayEvent `json:"events"`
	}
	q := url.Values{"topic": {topic}, "run_id": {runID}, "type": {"ok,failed"}, "after": {"0"}, "limit": {"1"}}
	if err := e.relayDo(ctx, http.MethodGet, "/v1/events?"+q.Encode(), nil, &resp); err != nil {
		log.Printf("actions: relay: %v", err)
		return
	}
	for _, ev := range resp.Events {
		e.jobEnded(ev)
	}
}

func (e *engine) endAction(t *Task, a *Action, state, output, errText string) {
	if err := e.store.write(logRecord{Op: "action_ended", Task: t.ID, ActionRun: a.Run, Value: state, Output: output, Text: errText}); err != nil {
		log.Printf("task %s: %v", t.ID, err)
	}
	typ := map[string]string{actionDone: "action_ok", actionFailed: "action_failed", actionNoReply: "action_no_reply"}[state]
	e.publish(t.Type, relay.Event{Type: typ, RunID: a.Run, Data: e.actionData(t, a.Tool, output, errText)})
	log.Printf("task %s: action %s %s%s", t.ID, a.Tool, state, errSuffix(errText))
}

func (e *engine) actionData(t *Task, tool, result, errText string) map[string]any {
	d := e.eventData(t, nil, nil)
	d["tool"] = tool
	if result != "" {
		if len(result) > maxEventText {
			result = result[:maxEventText] + "…"
		}
		d["result"] = result
	}
	if errText != "" {
		d["error"] = errText
	}
	return d
}

// --- long jobs ----------------------------------------------------------------

// taskOfJob is the task whose action started the job an event belongs to, or
// nil.
func (e *engine) taskOfJob(ev relayEvent) *Task {
	if ev.RunID == "" {
		return nil
	}
	found := e.store.find(func(t *Task) bool {
		return slices.ContainsFunc(t.Actions, func(a *Action) bool { return a.JobRunID == ev.RunID && a.JobTopic == ev.Topic })
	})
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

// followJobs reads every ok and failed event on relay through one consumer
// and ends the action whose job it is. The cursor is on relay, so a job
// that ended while foreman was down is still matched.
func (e *engine) followJobs(ctx context.Context) {
	backoff := time.Second
	retry := func(err error) {
		log.Printf("actions: relay: %v (retrying in %s)", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	for ctx.Err() == nil {
		err := e.relayDo(ctx, http.MethodPut, "/v1/consumers/"+jobsConsumer, map[string]string{"topic": "*", "type": "ok,failed"}, nil)
		if err == nil {
			break
		}
		retry(err)
	}
	for ctx.Err() == nil {
		var resp struct {
			Events []relayEvent `json:"events"`
		}
		if err := e.relayDo(ctx, http.MethodGet, "/v1/consumers/"+jobsConsumer+"/next?limit=100&wait="+pollWait, nil, &resp); err != nil {
			if ctx.Err() == nil {
				retry(err)
			}
			continue
		}
		if len(resp.Events) == 0 {
			continue
		}
		for _, ev := range resp.Events {
			e.jobEnded(ev)
		}
		last := resp.Events[len(resp.Events)-1].ID
		for ctx.Err() == nil {
			err := e.relayDo(ctx, http.MethodPost, "/v1/consumers/"+jobsConsumer+"/ack", map[string]uint64{"id": last}, nil)
			if err == nil {
				break
			}
			retry(err)
		}
		backoff = time.Second
	}
}

// jobEnded ends the action waiting for this job. A late outcome, after the
// wait ran out, is still recorded.
func (e *engine) jobEnded(ev relayEvent) {
	t := e.taskOfJob(ev)
	if t == nil {
		return
	}
	for _, a := range t.Actions {
		if a.JobRunID != ev.RunID || a.JobTopic != ev.Topic || (a.State != actionWaiting && a.State != actionNoReply) {
			continue
		}
		if ev.Type == "ok" {
			b, _ := json.Marshal(ev.Data)
			e.endAction(t, a, actionDone, string(b), "")
		} else {
			msg, _ := ev.Data["error"].(string)
			if msg == "" {
				msg = "the job failed"
			}
			e.endAction(t, a, actionFailed, "", msg)
		}
	}
}

// watchDeadlines moves waiting actions whose job took longer than its wait
// to no_reply.
func (e *engine) watchDeadlines(ctx context.Context) {
	tick := time.NewTicker(deadlineEvery)
	defer tick.Stop()
	for {
		now := time.Now()
		for _, t := range e.store.find(func(t *Task) bool { return len(t.Actions) > 0 }) {
			for _, a := range t.Actions {
				if a.State == actionWaiting && now.Sub(a.Called) > time.Duration(a.WaitMS)*time.Millisecond {
					e.endAction(t, a, actionNoReply, "",
						fmt.Sprintf("job %s didn't end within %s; it may still run", a.JobRunID, time.Duration(a.WaitMS)*time.Millisecond))
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// actionFilter reports whether a task's current action is in one of the
// states given (comma-separated).
func actionFilter(t *Task, states string) bool {
	a := t.action()
	return a != nil && slices.Contains(strings.Split(states, ","), a.State)
}
