package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() { deadlineEvery = 20 * time.Millisecond }

func text(s string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}, IsError: isErr}
}

var titleSchema = map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}},
	"required": []any{"title"}, "additionalProperties": false}

// actionTools are a repo server with a PR tool and its preview, and a job
// server whose tool starts a job on relay, on topic duckpipe.ingest so the
// test task type triggers on its failures.
func actionTools(jobWait string) []fakeTool {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	return []fakeTool{
		{&mcp.Tool{Name: "repo__check_edits", InputSchema: titleSchema, Annotations: ro}, func(a map[string]any) *mcp.CallToolResult {
			if a["title"] == "bad" {
				return text("edit 1: old text not found", true)
			}
			return text("--- a/models.yml\n+++ b/models.yml\n+measure", false)
		}},
		{&mcp.Tool{Name: "repo__open_pull_request", Description: "Open a PR.", InputSchema: titleSchema,
			Meta: mcp.Meta{"preview": "check_edits"}}, func(a map[string]any) *mcp.CallToolResult {
			if a["title"] == "fail" {
				return text("GitHub said 502", true)
			}
			return text("https://github.com/x/y/pull/7", false)
		}},
		{&mcp.Tool{Name: "repo__unsafe_preview", InputSchema: titleSchema, Meta: mcp.Meta{"preview": "open_pull_request"}},
			func(map[string]any) *mcp.CallToolResult { return text("done", false) }},
		{&mcp.Tool{Name: "jobs__start", InputSchema: titleSchema, Meta: mcp.Meta{"wait": jobWait}}, func(map[string]any) *mcp.CallToolResult {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "started job-1"}},
				StructuredContent: map[string]any{"run_id": "job-1", "topic": "duckpipe.ingest"}}
		}},
	}
}

func actionSpec(hub *fakeHub, model *ModelConf) runSpec {
	spec := testSpec(hub, model)
	spec.Type.Actions = []string{"repo__open_pull_request", "repo__unsafe_preview", "jobs__start"}
	return spec
}

func TestRunProposesAction(t *testing.T) {
	hub := newFakeHub(t, actionTools("1h")...)
	model := newFakeModel(t,
		openAI("", [2]string{"repo__open_pull_request", `{"title":"Add a measure"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	var tr transcript
	res, err := run(context.Background(), actionSpec(hub, openAIConf(model)), tr.rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.Action == nil || res.Action.Tool != "repo__open_pull_request" ||
		string(res.Action.Args) != `{"title":"Add a measure"}` || !strings.Contains(res.Action.Preview, "+measure") {
		t.Fatalf("result = %+v, action = %+v", res, res.Action)
	}
	if res.Action.WaitMS != defaultActionWait.Milliseconds() {
		t.Errorf("wait = %d", res.Action.WaitMS)
	}
	// Proposed, previewed, never called.
	if hub.count("repo__open_pull_request") != 0 || hub.count("repo__check_edits") != 1 {
		t.Errorf("hub calls = %v", hub.calls)
	}
	msgs := model.request(1)["messages"].([]any)
	if last := msgs[len(msgs)-1].(map[string]any); last["content"] != proposedText {
		t.Errorf("tool result = %v", last)
	}
	// The action tools are offered with the note; the other write tool is withheld.
	var desc string
	for _, tool := range model.request(0)["tools"].([]any) {
		if f := tool.(map[string]any)["function"].(map[string]any); f["name"] == "repo__open_pull_request" {
			desc = f["description"].(string)
		}
	}
	if !strings.HasSuffix(desc, actionNote) {
		t.Errorf("description = %q", desc)
	}
	start := tr.kind("start")[0]
	if strings.Join(start["actions"].([]string), ",") != "jobs__start,repo__open_pull_request,repo__unsafe_preview" ||
		strings.Join(start["withheld"].([]string), ",") != "url-to-bucket__download" {
		t.Errorf("start = %v", start)
	}
	if tool := tr.kind("tool")[0]; tool["proposed"] != true || tool["preview"] == nil {
		t.Errorf("tool record = %v", tool)
	}
}

func TestRunRefusesBadProposals(t *testing.T) {
	hub := newFakeHub(t, actionTools("1h")...)
	model := newFakeModel(t,
		openAI("", [2]string{"repo__open_pull_request", `{"name":"x"}`}),    // doesn't fit the schema
		openAI("", [2]string{"repo__open_pull_request", `{"title":"bad"}`}), // the preview fails
		openAI("", [2]string{"repo__unsafe_preview", `{"title":"x"}`}),      // its preview isn't read-only
		openAI("", [2]string{"repo__open_pull_request", `{"title":"good"}`}),
		openAI("", [2]string{"jobs__start", `{"title":"second"}`}), // one per run
		openAI("", [2]string{"submit", goodSubmit}))
	spec := actionSpec(hub, openAIConf(model))
	spec.Agent.MaxSteps = 10
	var tr transcript
	res, _ := run(context.Background(), spec, tr.rec)
	if res.Status != "ok" || res.Action == nil || string(res.Action.Args) != `{"title":"good"}` {
		t.Fatalf("result = %+v, action = %+v", res, res.Action)
	}
	tools := tr.kind("tool")
	want := []string{"input schema", "old text not found", "isn't read-only", proposedText, "already proposed"}
	for i, w := range want {
		if !strings.Contains(tools[i]["result"].(string), w) || tools[i]["error"] != (w != proposedText) {
			t.Errorf("call %d: %v, want %q", i, tools[i], w)
		}
	}
	// The schema check comes first, so the preview ran for "bad" and "good" only.
	if hub.count("repo__check_edits") != 2 || hub.count("repo__open_pull_request") != 0 || hub.count("jobs__start") != 0 {
		t.Errorf("hub calls = %v", hub.calls)
	}
}

// actionEnv is a harness whose task type may propose the action tools.
func actionEnv(t *testing.T, jobWait string, script ...string) *harness {
	env := newTestEnvHub(t, newFakeModel(t, script...), 0, newFakeHub(t, actionTools(jobWait)...))
	env.e.defs.Types["duckpipe-failure"].Actions = []string{"repo__open_pull_request", "jobs__start"}
	return env
}

// proposedTask runs an event's task to its answer and returns it.
func proposedTask(t *testing.T, env *harness) *Task {
	t.Helper()
	env.relay.add(duckEvent(1, "orders"))
	env.e.start(env.ctx)
	var tk *Task
	eventually(t, "the task is answered", func() bool {
		ts := env.tasks()
		if len(ts) == 1 && ts[0].Status == statusAnswered {
			tk = ts[0]
		}
		return tk != nil
	})
	return tk
}

func TestActionApproveQuick(t *testing.T) {
	env := actionEnv(t, "",
		openAI("", [2]string{"repo__open_pull_request", `{"title":"Add a measure"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	tk := proposedTask(t, env)
	a := tk.action()
	if a == nil || a.State != actionProposed || a.Run != tk.Runs[0].ID || a.Agent != "oncall" || a.Preview == "" {
		t.Fatalf("action = %+v", a)
	}
	if got := env.e.store.find(func(t *Task) bool { return actionFilter(t, "proposed") }); len(got) != 1 {
		t.Errorf("proposed filter found %d", len(got))
	}
	if err := env.e.decideAction(env.ctx, tk.ID, "approve", "looks right"); err != nil {
		t.Fatal(err)
	}
	// A second click while it runs or after it is done is refused.
	if err := env.e.decideAction(env.ctx, tk.ID, "approve", ""); err == nil {
		t.Error("a second approve was accepted")
	}
	eventually(t, "the action is done", func() bool { return env.st.get(tk.ID).action().State == actionDone })
	a = env.st.get(tk.ID).action()
	if a.Result != "https://github.com/x/y/pull/7" || a.Note != "looks right" || a.Attempts != 1 {
		t.Errorf("action = %+v", a)
	}
	if env.hub.count("repo__open_pull_request") != 1 {
		t.Errorf("calls = %v", env.hub.calls)
	}
	if !slices.Contains(env.hub.runIDs, a.Run) {
		t.Errorf("the hub never saw the proposing run's id %s: %v", a.Run, env.hub.runIDs)
	}
	eventually(t, "action events are published", func() bool {
		ty := env.relay.types()
		return slices.Contains(ty, "action_proposed") && slices.Contains(ty, "action_ok")
	})
	if st := env.st.get(tk.ID).Status; st != statusAnswered {
		t.Errorf("the task's status changed to %s", st)
	}
}

func TestActionRejectAndRetry(t *testing.T) {
	env := actionEnv(t, "",
		openAI("", [2]string{"repo__open_pull_request", `{"title":"fail"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	tk := proposedTask(t, env)
	if err := env.e.decideAction(env.ctx, tk.ID, "maybe", ""); err == nil {
		t.Error("a bad decision was accepted")
	}

	// A failed call can be approved again; a rejected action can't.
	env.e.decideAction(env.ctx, tk.ID, "approve", "")
	eventually(t, "the call fails", func() bool { return env.st.get(tk.ID).action().State == actionFailed })
	if a := env.st.get(tk.ID).action(); a.Error != "GitHub said 502" {
		t.Errorf("action = %+v", a)
	}
	if err := env.e.decideAction(env.ctx, tk.ID, "reject", ""); err == nil {
		t.Error("a failed action was rejected")
	}
	env.e.decideAction(env.ctx, tk.ID, "approve", "")
	eventually(t, "the second call fails", func() bool {
		a := env.st.get(tk.ID).action()
		return a.State == actionFailed && a.Attempts == 2
	})
	if env.hub.count("repo__open_pull_request") != 2 {
		t.Errorf("calls = %v", env.hub.calls)
	}
}

func TestActionReject(t *testing.T) {
	env := actionEnv(t, "",
		openAI("", [2]string{"repo__open_pull_request", `{"title":"x"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	tk := proposedTask(t, env)
	if err := env.e.decideAction(env.ctx, tk.ID, "reject", "wrong file"); err != nil {
		t.Fatal(err)
	}
	if a := env.st.get(tk.ID).action(); a.State != actionRejected || a.Note != "wrong file" {
		t.Errorf("action = %+v", a)
	}
	if err := env.e.decideAction(env.ctx, tk.ID, "approve", ""); err == nil {
		t.Error("a rejected action was approved")
	}
	time.Sleep(50 * time.Millisecond)
	if env.hub.count("repo__open_pull_request") != 0 {
		t.Errorf("calls = %v", env.hub.calls)
	}
}

func TestActionLongJobAndLoopGuard(t *testing.T) {
	env := actionEnv(t, "1h",
		openAI("", [2]string{"jobs__start", `{"title":"again"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	tk := proposedTask(t, env)
	env.e.decideAction(env.ctx, tk.ID, "approve", "")
	eventually(t, "the action waits for its job", func() bool { return env.st.get(tk.ID).action().State == actionWaiting })
	if a := env.st.get(tk.ID).action(); a.JobRunID != "job-1" || a.JobTopic != "duckpipe.ingest" {
		t.Fatalf("action = %+v", a)
	}

	// The job fails. The event ends the action, and though it matches the
	// task type, it is attached to the task instead of starting a new one.
	ev := duckEvent(2, "other-pipe")
	ev.RunID, ev.Data["error"] = "job-1", "still broken"
	env.relay.add(ev)
	eventually(t, "the action failed", func() bool { return env.st.get(tk.ID).action().State == actionFailed })
	if a := env.st.get(tk.ID).action(); a.Error != "still broken" {
		t.Errorf("action = %+v", a)
	}
	eventually(t, "the event is attached", func() bool { return slices.Contains(env.st.get(tk.ID).Events, 2) })
	if n := len(env.tasks()); n != 1 {
		t.Errorf("%d tasks, want 1", n)
	}
	if len(env.st.get(tk.ID).Runs) != 1 {
		t.Error("the job's event started a run")
	}
}

func TestActionNoReplyThenLateOutcome(t *testing.T) {
	env := actionEnv(t, "30ms",
		openAI("", [2]string{"jobs__start", `{"title":"again"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	tk := proposedTask(t, env)
	env.e.decideAction(env.ctx, tk.ID, "approve", "")
	eventually(t, "no reply", func() bool { return env.st.get(tk.ID).action().State == actionNoReply })
	env.relay.add(relayEvent{ID: 5, Topic: "duckpipe.ingest", Type: "ok", RunID: "job-1", Data: map[string]any{"rows": 3}})
	eventually(t, "the late outcome is recorded", func() bool { return env.st.get(tk.ID).action().State == actionDone })
	if a := env.st.get(tk.ID).action(); a.Result != `{"rows":3}` {
		t.Errorf("action = %+v", a)
	}
}

func TestActionJobEndedBeforeItWasRecorded(t *testing.T) {
	env := actionEnv(t, "1h",
		openAI("", [2]string{"jobs__start", `{"title":"again"}`}),
		openAI("", [2]string{"submit", goodSubmit}))
	tk := proposedTask(t, env)
	// Already on relay, and acked by the consumer before the call.
	env.relay.add(relayEvent{ID: 5, Topic: "duckpipe.ingest", Type: "ok", RunID: "job-1"})
	eventually(t, "the consumer passed it", func() bool { return env.relay.acked(jobsConsumer) == 5 })
	env.e.decideAction(env.ctx, tk.ID, "approve", "")
	eventually(t, "the look-back matches it", func() bool { return env.st.get(tk.ID).action().State == actionDone })
}

func TestActionFromFailedRunIsDropped(t *testing.T) {
	env := actionEnv(t, "",
		openAI("", [2]string{"repo__open_pull_request", `{"title":"x"}`}),
		openAI("I give up."))
	env.relay.add(duckEvent(1, "orders"))
	env.e.start(env.ctx)
	eventually(t, "the run fails", func() bool { ts := env.tasks(); return len(ts) == 1 && ts[0].Status == statusFailed })
	if tk := env.tasks()[0]; tk.action() != nil {
		t.Errorf("action = %+v", tk.action())
	}
	if err := env.e.decideAction(env.ctx, env.tasks()[0].ID, "approve", ""); err == nil {
		t.Error("approved an action that doesn't exist")
	}
}

func TestActionStore(t *testing.T) {
	dir := t.TempDir()
	st, _ := openStore(dir)
	tk := &Task{ID: "t1", Type: "x", Status: statusCreated, Runs: []*RunInfo{}}
	st.write(logRecord{Op: "created", Task: "t1", New: tk})
	p := &proposal{Tool: "a__b", Args: json.RawMessage(`{}`), WaitMS: 1000}
	st.write(logRecord{Op: "run_ended", Task: "t1", Run: &RunInfo{ID: "r1", Agent: "oncall", Status: "ok"}, Action: p})
	st.write(logRecord{Op: "action_decided", Task: "t1", ActionRun: "r1", Value: "approve"})
	st.log.Close()

	// A restart while calling: failed, not called again.
	st, _ = openStore(dir)
	e := newEngine(&Defs{Types: map[string]*TaskType{}, Agents: map[string]*Agent{}}, st, nil, "", "", 1)
	e.resumeActions(context.Background())
	if a := st.get("t1").action(); a.State != actionFailed || !strings.Contains(a.Error, "interrupted") {
		t.Errorf("action = %+v", a)
	}
	// Run again with a new proposal supersedes the failed one.
	st.write(logRecord{Op: "run_ended", Task: "t1", Run: &RunInfo{ID: "r2", Agent: "oncall", Status: "ok"}, Action: p})
	got := st.get("t1")
	if len(got.Actions) != 2 || got.Actions[0].State != actionSuperseded || got.action().State != actionProposed {
		t.Errorf("actions = %+v %+v", got.Actions[0], got.Actions[1])
	}
}

func TestCheckActions(t *testing.T) {
	tools := map[string]*mcp.Tool{}
	for _, x := range actionTools("") {
		tools[x.tool.Name] = x.tool
	}
	tools["vigil__get_run"] = &mcp.Tool{Name: "vigil__get_run", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}
	for actions, want := range map[string]string{
		"repo__open_pull_request,jobs__start": "",
		"repo__nope":                          "not in profile",
		"vigil__get_run":                      "read-only",
		"repo__unsafe_preview":                "preview",
	} {
		err := checkActions(&TaskType{Name: "t", Actions: strings.Split(actions, ",")}, "oncall", tools)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", actions, err, want)
		}
	}
}
