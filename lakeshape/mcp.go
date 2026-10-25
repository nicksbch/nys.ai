package main

// The MCP tools, at /mcp on -listen, so an agent can see the jobs and run
// one now through mcp-hub (lakeshape__*). run_job asks the service through
// relay, as `lakeshape trigger` does, and returns at once; the run reports
// on lakeshape.run with its run_id. See SPEC-mcp.md.

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	relay "relay/client"
)

// board is what the tools see of the service: the jobs it has loaded, when
// each last ran, and which are running. The service's loop and runs update
// it; the tools only read it.
type board struct {
	bus *relay.Client // nil without relay

	mu      sync.Mutex
	order   []string
	jobs    map[string]*job
	last    map[string]time.Time
	running map[string]bool
}

func newBoard(bus *relay.Client) *board {
	return &board{bus: bus, jobs: map[string]*job{}, last: map[string]time.Time{}, running: map[string]bool{}}
}

// setJobs records the jobs of a project the service has just loaded.
func (b *board) setJobs(order []string, jobs map[string]*job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order, b.jobs = order, jobs
}

// setLast records when jobs last ran, from run_history.
func (b *board) setLast(last map[string]time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, t := range last {
		b.last[name] = t
	}
}

// start marks jobs running, from now; it returns the function that marks
// them done.
func (b *board) start(names []string) func() {
	now := time.Now()
	b.mu.Lock()
	for _, name := range names {
		b.running[name], b.last[name] = true, now
	}
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, name := range names {
			delete(b.running, name)
		}
	}
}

func (b *board) has(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.jobs[name] != nil
}

type jobView struct {
	Name        string     `json:"name"`
	Select      string     `json:"select,omitempty"`
	Exclude     string     `json:"exclude,omitempty"`
	Schedule    string     `json:"schedule,omitempty"` // 5-field cron, in the project's time zone
	On          []trigger  `json:"on,omitempty"`
	FullRefresh bool       `json:"full_refresh,omitempty"`
	LastRun     *time.Time `json:"last_run,omitempty"` // when it last started
	Running     bool       `json:"running"`
}

type jobsOut struct {
	Jobs []jobView `json:"jobs"`
}

type runJobIn struct {
	Job         string `json:"job" jsonschema:"the job's name, from list_jobs"`
	FullRefresh bool   `json:"full_refresh,omitempty" jsonschema:"rebuild incremental models from scratch; much slower"`
}

type runJobOut struct {
	RunID string `json:"run_id"`
	Topic string `json:"topic"`
}

// serveMCP serves /mcp and /healthz on addr until ctx ends.
func (b *board) serveMCP(ctx context.Context, addr, token string) {
	srv := &http.Server{Addr: addr, Handler: b.handler(token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("mcp on http://%s/mcp", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (b *board) handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	if token != "" {
		mux.Handle("/mcp", bearerAuth(token, b.mcpHandler()))
	} else {
		log.Print("LAKESHAPE_MCP_TOKEN is not set: no MCP tools at /mcp")
	}
	return mux
}

// bearerAuth requires "Authorization: Bearer <token>".
func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (b *board) mcpHandler() http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "lakeshape", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "list_jobs", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "The jobs the service runs (jobs.yml): name, the models they build (select, exclude), cron " +
			"schedule, the relay events that trigger them (on), when each last started, and whether it is running now."}, b.toolJobs)
	mcp.AddTool(s, &mcp.Tool{Name: "run_job",
		Description: "Run a job now, on prod, as `lakeshape trigger` does. It returns at once with the run's run_id; " +
			"the run then reports started, and ok or failed, on relay topic lakeshape.run with that run_id. It runs " +
			"after the run under way, if any. full_refresh rebuilds incremental models from scratch and can take much longer.",
		Meta: mcp.Meta{"wait": "2h"}}, b.toolRunJob)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
}

func (b *board) toolJobs(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, jobsOut, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := jobsOut{Jobs: []jobView{}}
	for _, name := range b.order {
		j := b.jobs[name]
		v := jobView{Name: name, Select: j.Select, Exclude: j.Exclude, Schedule: j.Schedule, On: j.On,
			FullRefresh: j.FullRefresh, Running: b.running[name]}
		if t, ok := b.last[name]; ok {
			v.LastRun = &t
		}
		out.Jobs = append(out.Jobs, v)
	}
	return nil, out, nil
}

func (b *board) toolRunJob(ctx context.Context, req *mcp.CallToolRequest, in runJobIn) (*mcp.CallToolResult, runJobOut, error) {
	if !b.has(in.Job) {
		return nil, runJobOut{}, fmt.Errorf("there is no job %q: list_jobs lists them", in.Job)
	}
	if b.bus == nil {
		return nil, runJobOut{}, fmt.Errorf("relay is not configured (RELAY_URL or %s): run_job asks the service through relay", relay.EnvFile)
	}
	runID, _, err := requestRun(ctx, b.bus, in.Job, in.FullRefresh)
	if err != nil {
		return nil, runJobOut{}, err
	}
	log.Printf("mcp: job %s requested (run %s)", in.Job, runID)
	return nil, runJobOut{RunID: runID, Topic: "lakeshape.run"}, nil
}
