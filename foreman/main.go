// foreman runs always-on agents for the server. Something happens (a relay
// event, a schedule, you), foreman turns it into a task, and an agent (a
// model with the tools of one mcp-hub profile) works on it until it submits
// a result. See SPEC.md.
//
// Usage:
//
//	foreman serve                                        # the service: triggers, tasks, runs
//	foreman try duckpipe-failure -event failed.json     # one run, transcript on stdout
//	foreman check                                        # validate files, reach hub and models
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	relay "relay/client"
)

const usageText = `foreman: always-on agents for the server.

Commands:
  foreman serve [-listen 127.0.0.1:8096] [-data DIR] [-concurrency 2] [-retention 2160h]
      Run the service: relay events and schedules create tasks, and agents
      work on them. Tasks and transcripts are kept in -data. Relay comes from
      RELAY_URL and RELAY_TOKEN, or /etc/relay-client.env. The web page is
      at /ui, behind FOREMAN_AUTH (user:pass); its JSON API is under /api/.
  foreman try <task type> [-input TEXT | -event FILE] [-agent A] [-model M]
      Run one task now, in the foreground. The transcript is printed as JSON
      lines on stdout and the result on stderr. Nothing is stored.
      Exit codes: 0 result submitted, 1 run failed, 2 bad files or the hub or
      model could not be reached.
  foreman check
      Validate the files, reach each agent's mcp-hub profile, and test each
      model with one tool call. Exit codes: 0 all good, 1 something is
      unreachable or a model didn't call the tool, 2 bad files or an action
      that isn't in its agent's profile.

Common flags:
  -config DIR   models.json, agents/ and tasks/ (default /etc/foreman)
  -hub URL      mcp-hub (default http://127.0.0.1:8094); token from MCP_HUB_TOKEN
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(ctx, os.Args[2:]))
	case "try":
		os.Exit(try(ctx, os.Args[2:]))
	case "check":
		os.Exit(check(ctx, os.Args[2:]))
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
}

type common struct {
	config, hub string
}

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.config, "config", "/etc/foreman", "directory with models.json, agents/ and tasks/")
	fs.StringVar(&c.hub, "hub", "http://127.0.0.1:8094", "mcp-hub URL")
}

func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func try(ctx context.Context, args []string) int {
	var c common
	fs := flag.NewFlagSet("try", flag.ContinueOnError)
	c.flags(fs)
	input := fs.String("input", "", "the task's input text")
	event := fs.String("event", "", "a file holding a relay event (JSON), as the trigger")
	agentName := fs.String("agent", "", "use this agent instead of the task type's")
	modelName := fs.String("model", "", "use this model (a name from models.json) instead of the agent's")
	// The task type may come before or after the flags.
	var typeName string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		typeName, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if typeName == "" && fs.NArg() > 0 {
		typeName = fs.Arg(0)
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
		return 2
	}
	if typeName == "" || (*input == "") == (*event == "") {
		return fail("usage: foreman try <task type> (-input TEXT | -event FILE) [-agent A] [-model M]")
	}

	defs, err := loadDefs(c.config, os.Getenv)
	if err != nil {
		return fail("%v", err)
	}
	tt := defs.Types[typeName]
	if tt == nil {
		return fail("no task type %q in %s/tasks", typeName, c.config)
	}
	agent := defs.Agents[tt.Agent]
	if *agentName != "" {
		if agent = defs.Agents[*agentName]; agent == nil {
			return fail("no agent %q in %s/agents", *agentName, c.config)
		}
	}
	modelKey := agent.Model
	if *modelName != "" {
		modelKey = *modelName
	}
	model := defs.Models[modelKey]
	if model == nil {
		return fail("no model %q in models.json", modelKey)
	}
	text := *input
	if *event != "" {
		if text, err = eventInput(*event); err != nil {
			return fail("%v", err)
		}
	}

	enc := json.NewEncoder(os.Stdout)
	rec := func(kind string, data map[string]any) {
		data["kind"], data["t"] = kind, time.Now().UTC()
		enc.Encode(data)
	}
	res, err := run(ctx, runSpec{RunID: newRunID(), Agent: agent, Type: tt, Model: model, ModelKey: modelKey,
		Input: text, HubURL: c.hub, HubToken: os.Getenv("MCP_HUB_TOKEN")}, rec)
	if err != nil {
		return fail("could not run: %v", err)
	}
	fmt.Fprintf(os.Stderr, "\n%s after %d steps (%d tokens in, %d out, %d read from cache)\n",
		res.Status, res.Steps, res.Usage.In, res.Usage.Out, res.Usage.CacheRead)
	if res.Status != "ok" {
		fmt.Fprintf(os.Stderr, "error: %s\n", res.Error)
		return 1
	}
	for _, f := range tt.Output {
		fmt.Fprintf(os.Stderr, "\n## %s\n%s\n", f, res.Result[f])
	}
	return 0
}

// eventInput is the first user message for a relay event: the event as JSON.
func eventInput(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var ev map[string]any
	if err := json.Unmarshal(b, &ev); err != nil {
		return "", fmt.Errorf("%s: not a JSON object: %v", path, err)
	}
	pretty, _ := json.MarshalIndent(ev, "", "  ")
	return "Event:\n" + string(pretty), nil
}

func check(ctx context.Context, args []string) int {
	var c common
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	c.flags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	defs, err := loadDefs(c.config, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	enc := json.NewEncoder(os.Stdout)
	rc := 0
	var profiles, models []string
	for _, a := range defs.Agents {
		if !slices.Contains(profiles, a.Profile) {
			profiles = append(profiles, a.Profile)
		}
	}
	for name := range defs.Models {
		models = append(models, name)
	}
	slices.Sort(profiles)
	slices.Sort(models)
	enc.Encode(map[string]any{"check": "files", "agents": len(defs.Agents), "task_types": len(defs.Types), "models": len(models)})

	listed := map[string]*toolSet{} // by profile, when reachable
	for _, p := range profiles {
		spec := runSpec{RunID: "check", Agent: &Agent{Profile: p}, HubURL: c.hub, HubToken: os.Getenv("MCP_HUB_TOKEN")}
		line := map[string]any{"check": "profile", "name": p}
		hub, err := connectHub(ctx, spec)
		if err == nil {
			var ts *toolSet
			ts, err = hubTools(ctx, hub, nil)
			hub.Close()
			if err == nil {
				listed[p] = ts
				line["tools"], line["withheld"] = len(ts.offered), ts.withheld
			}
		}
		if err != nil {
			line["error"], rc = err.Error(), 1
		}
		enc.Encode(line)
	}
	types := make([]string, 0, len(defs.Types))
	for name := range defs.Types {
		types = append(types, name)
	}
	slices.Sort(types)
	for _, name := range types {
		tt := defs.Types[name]
		ts := listed[defs.Agents[tt.Agent].Profile]
		if len(tt.Actions) == 0 || ts == nil {
			continue // nothing to check, or the profile is unreachable (reported above)
		}
		line := map[string]any{"check": "actions", "task_type": name, "actions": tt.Actions}
		if err := checkActions(tt, defs.Agents[tt.Agent].Profile, ts.all); err != nil {
			line["error"], rc = err.Error(), 2
		}
		enc.Encode(line)
	}
	for _, name := range models {
		line := map[string]any{"check": "model", "name": name}
		if err := testModel(ctx, defs.Models[name]); err != nil {
			line["error"], rc = err.Error(), 1
		} else {
			line["tools"] = "ok"
		}
		enc.Encode(line)
	}
	return rc
}

// checkActions refuses a task type's action that isn't in its agent's
// profile, is read-only (so not an action), or names a preview that is
// missing or not read-only.
func checkActions(tt *TaskType, profile string, tools map[string]*mcp.Tool) error {
	for _, name := range tt.Actions {
		t := tools[name]
		switch {
		case t == nil:
			return fmt.Errorf("tasks/%s.md: action %s is not in profile %s", tt.Name, name, profile)
		case readOnly(t):
			return fmt.Errorf("tasks/%s.md: action %s is read-only, so it is offered anyway; remove it from actions", tt.Name, name)
		}
		if p := previewName(t); p != "" && (tools[p] == nil || !readOnly(tools[p])) {
			return fmt.Errorf("tasks/%s.md: action %s: its preview %s is missing from profile %s or not read-only", tt.Name, name, p, profile)
		}
	}
	return nil
}

// testModel asks the model to call a tool once: it shows the endpoint is up
// and that it accepts tools (llama-server needs its chat template for that).
func testModel(ctx context.Context, m *ModelConf) error {
	ping := toolDef{Name: "ping", Description: "Answers pong.", Schema: map[string]any{"type": "object", "properties": map[string]any{}}}
	t, err := newSession(m, "You are checking that tool calling works.", []toolDef{ping}).next(ctx, nil, "Call the ping tool now.")
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(t.Calls, func(c toolCall) bool { return c.Name == "ping" }) {
		return errors.New("the model answered without calling the tool: " + strings.TrimSpace(t.Text))
	}
	return nil
}

func serve(ctx context.Context, args []string) int {
	var c common
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	c.flags(fs)
	listen := fs.String("listen", "127.0.0.1:8096", "address to serve on")
	data := fs.String("data", envOr("STATE_DIRECTORY", "./data"), "directory for tasks and transcripts")
	concurrency := fs.Int("concurrency", 2, "most runs at the same time")
	retention := fs.Duration("retention", 90*24*time.Hour, "how long closed tasks are kept")
	auth := fs.String("auth", os.Getenv("FOREMAN_AUTH"), "user:pass for the web page and API")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !strings.Contains(*auth, ":") {
		log.Print("FOREMAN_AUTH (or -auth) must be set as user:pass: the web page is never served without a password")
		return 2
	}
	if *concurrency < 1 {
		log.Print("-concurrency must be at least 1")
		return 2
	}
	defs, err := loadDefs(c.config, os.Getenv)
	if err != nil {
		log.Print(err)
		return 2
	}
	st, err := openStore(*data)
	if err != nil {
		log.Print(err)
		return 2
	}
	bus := relay.FromEnv()
	if bus == nil {
		log.Print("relay is not configured (RELAY_URL): on_event triggers are off and nothing is published")
	}
	e := newEngine(defs, st, bus, c.hub, os.Getenv("MCP_HUB_TOKEN"), *concurrency)

	go func() { // retention, now and daily
		for {
			if n, err := st.expire(time.Now().Add(-*retention)); err != nil {
				log.Printf("retention: %v", err)
			} else if n > 0 {
				log.Printf("retention: removed %d closed tasks older than %s", n, *retention)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(24 * time.Hour):
			}
		}
	}()
	e.start(ctx)

	srv := &http.Server{Addr: *listen, Handler: e.routes(ctx, *auth), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("foreman listening on http://%s (%d agents, %d task types, data in %s)", *listen, len(defs.Agents), len(defs.Types), *data)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
		return 1
	}
	e.wait()
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
