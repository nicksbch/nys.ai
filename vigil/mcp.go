package main

// MCP tools at /mcp, so an agent can ask what vigil knows. Every tool only
// reads what the monitor already holds in memory. See MCP.md.

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultPoints = 60
	maxPoints     = 240
	defaultLimit  = 50
	maxLimit      = 500
)

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

func (m *monitor) mcpHandler() http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "vigil", Version: "1"}, nil)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	window := shortDur(m.events.window)

	mcp.AddTool(s, &mcp.Tool{Name: "get_system", Annotations: readOnly,
		Description: "The server right now: CPU, memory, 1-minute load and each watched disk, " +
			"with the thresholds at which vigil raises an alert."}, m.toolSystem)
	mcp.AddTool(s, &mcp.Tool{Name: "get_system_history", Annotations: readOnly,
		Description: "CPU, memory and load over the last `since` (up to 24h), averaged into `points` samples. " +
			"Disk history is not kept."}, m.toolSystemHistory)
	mcp.AddTool(s, &mcp.Tool{Name: "list_alerts", Annotations: readOnly,
		Description: "Alerts firing now (disk full, CPU or memory high, a job not run when expected). " +
			"An empty list means nothing is firing. Failed jobs are not listed here: use list_jobs or list_events."}, m.toolAlerts)
	mcp.AddTool(s, &mcp.Tool{Name: "list_jobs", Annotations: readOnly,
		Description: "One row per app and job kind (e.g. url-to-bucket/download): totals over vigil's retention, " +
			"totals over the last 24 hours, failures over the last " + window + " (failures_recent), last success, " +
			"last failure with the first line of its error, and the expected interval if one is set."}, m.toolJobs)
	mcp.AddTool(s, &mcp.Tool{Name: "list_events", Annotations: readOnly,
		Description: "Job events, newest first: the last 24 hours by default, `since` up to " + window + ". " +
			"Errors are cut to one line: call get_run with the event's run_id for the full text."}, m.toolEvents)
	mcp.AddTool(s, &mcp.Tool{Name: "get_run", Annotations: readOnly,
		Description: "Every event of one run from the last " + window + ", oldest first, with the full error text. " +
			"finished is false when the run started and has not reported a result."}, m.toolRun)

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
}

// --- get_system ---------------------------------------------------------------

type systemOut struct {
	Host         string     `json:"host"`
	Now          time.Time  `json:"now"`
	VigilStarted time.Time  `json:"vigil_started"`
	CPUPct       *float64   `json:"cpu_pct" jsonschema:"% busy; null where it is not available"`
	MemPct       *float64   `json:"mem_pct" jsonschema:"% of memory in use, excluding cache; null where it is not available"`
	Load1        *float64   `json:"load1" jsonschema:"1-minute load average; null where it is not available"`
	Disks        []Disk     `json:"disks" jsonschema:"a disk that cannot be read has error set and its numbers at 0"`
	Thresholds   thresholds `json:"thresholds"`
}

type thresholds struct {
	DiskPct float64 `json:"disk_pct"`
	CPUPct  float64 `json:"cpu_pct"`
	MemPct  float64 `json:"mem_pct"`
	Sustain string  `json:"sustain" jsonschema:"how long CPU or memory must stay above its threshold"`
}

func (m *monitor) toolSystem(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, systemOut, error) {
	m.mu.Lock()
	var last Sample
	if len(m.samples) > 0 {
		last = m.samples[len(m.samples)-1]
	} else {
		last = Sample{CPU: -1, Mem: -1, Load1: -1}
	}
	disks := append([]Disk{}, m.disks...)
	m.mu.Unlock()
	return nil, systemOut{
		Host: m.host, Now: time.Now().UTC(), VigilStarted: m.started.UTC(),
		CPUPct: avail(last.CPU), MemPct: avail(last.Mem), Load1: avail(last.Load1),
		Disks: disks,
		Thresholds: thresholds{DiskPct: m.cfg.diskWarn, CPUPct: m.cfg.cpuWarn, MemPct: m.cfg.memWarn,
			Sustain: shortDur(m.cfg.sustain)},
	}, nil
}

// avail turns a sample value into nil when it is not available (-1).
func avail(v float64) *float64 {
	if v < 0 {
		return nil
	}
	return &v
}

// --- get_system_history -------------------------------------------------------

type historyIn struct {
	Since  string `json:"since,omitempty" jsonschema:"how far back, as a Go duration such as 90m or 6h; default 1h, at most 24h"`
	Points int    `json:"points,omitempty" jsonschema:"how many samples to return, 2-240; default 60"`
}

type historyOut struct {
	Interval string      `json:"interval" jsonschema:"how often vigil samples the system"`
	Samples  []sampleOut `json:"samples"`
}

type sampleOut struct {
	T      time.Time `json:"t"`
	CPUPct *float64  `json:"cpu_pct"`
	MemPct *float64  `json:"mem_pct"`
	Load1  *float64  `json:"load1"`
}

func (m *monitor) toolSystemHistory(ctx context.Context, req *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, historyOut, error) {
	since, err := parseSince(in.Since, time.Hour, history)
	if err != nil {
		return nil, historyOut{}, err
	}
	points := in.Points
	if points == 0 {
		points = defaultPoints
	}
	if points < 2 || points > maxPoints {
		return nil, historyOut{}, fmt.Errorf("points must be 2-%d", maxPoints)
	}
	cutoff := time.Now().Add(-since)
	m.mu.Lock()
	var window []Sample
	for i, s := range m.samples {
		if !s.Time.Before(cutoff) {
			window = m.samples[i:]
			break
		}
	}
	window = downsample(window, points)
	m.mu.Unlock()

	out := historyOut{Interval: m.cfg.interval.String(), Samples: make([]sampleOut, len(window))}
	for i, s := range window {
		out.Samples[i] = sampleOut{T: s.Time.UTC(), CPUPct: avail(s.CPU), MemPct: avail(s.Mem), Load1: avail(s.Load1)}
	}
	return nil, out, nil
}

// --- list_alerts --------------------------------------------------------------

type alertsOut struct {
	Alerts []Alert `json:"alerts"`
}

func (m *monitor) toolAlerts(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, alertsOut, error) {
	alerts := m.alerts.list()
	for i := range alerts {
		alerts[i].Since = alerts[i].Since.UTC()
	}
	return nil, alertsOut{Alerts: alerts}, nil
}

// --- list_jobs ----------------------------------------------------------------

type jobsIn struct {
	App string `json:"app,omitempty" jsonschema:"only this app (exact name); default all"`
}

type jobsOut struct {
	Jobs   []*statView `json:"jobs"`
	Recent string      `json:"recent" jsonschema:"the period failures_recent covers"`
}

func (m *monitor) toolJobs(ctx context.Context, req *mcp.CallToolRequest, in jobsIn) (*mcp.CallToolResult, jobsOut, error) {
	stats, recent := m.events.snapshot()
	out := jobsOut{Jobs: []*statView{}, Recent: shortDur(m.events.window)}
	for _, v := range m.jobs(stats, recent) {
		if in.App != "" && v.App != in.App {
			continue
		}
		if v.LastError != "" {
			v.LastError = summary(v.LastError)
		}
		v.LastSuccess, v.LastFailure = utcPtr(v.LastSuccess), utcPtr(v.LastFailure)
		out.Jobs = append(out.Jobs, v)
	}
	return nil, out, nil
}

// --- list_events --------------------------------------------------------------

type eventsIn struct {
	App    string `json:"app,omitempty" jsonschema:"only this app (exact name)"`
	Kind   string `json:"kind,omitempty" jsonschema:"only this kind (exact name)"`
	Status string `json:"status,omitempty" jsonschema:"only this status: started, ok or failed"`
	Since  string `json:"since,omitempty" jsonschema:"how far back, as a Go duration such as 90m, 6h or 72h; default 24h, at most what vigil keeps in memory (7 days unless set otherwise)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"most events to return, 1-500; default 50"`
}

type eventsOut struct {
	Events    []Event `json:"events"`
	Truncated bool    `json:"truncated" jsonschema:"true when limit stopped the list"`
}

func (m *monitor) toolEvents(ctx context.Context, req *mcp.CallToolRequest, in eventsIn) (*mcp.CallToolResult, eventsOut, error) {
	since, err := parseSince(in.Since, 24*time.Hour, m.events.window)
	if err != nil {
		return nil, eventsOut{}, err
	}
	switch in.Status {
	case "", statusStart, statusOK, statusFailed:
	default:
		return nil, eventsOut{}, fmt.Errorf(`status must be "started", "ok" or "failed"`)
	}
	limit := in.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return nil, eventsOut{}, fmt.Errorf("limit must be 1-%d", maxLimit)
	}

	_, recent := m.events.snapshot()
	cutoff := time.Now().Add(-since)
	out := eventsOut{Events: []Event{}}
	for i := len(recent) - 1; i >= 0; i-- {
		e := recent[i]
		if e.Time.Before(cutoff) {
			break
		}
		if (in.App != "" && e.App != in.App) || (in.Kind != "" && e.Kind != in.Kind) ||
			(in.Status != "" && e.Status != in.Status) {
			continue
		}
		if len(out.Events) == limit {
			out.Truncated = true
			break
		}
		if e.Error != "" {
			e.Error = summary(e.Error)
		}
		e.Time = e.Time.UTC()
		out.Events = append(out.Events, e)
	}
	return nil, out, nil
}

// --- get_run ------------------------------------------------------------------

type runIn struct {
	RunID string `json:"run_id" jsonschema:"the run_id of an event"`
}

type runOut struct {
	RunID      string  `json:"run_id"`
	Events     []Event `json:"events"`
	Finished   *bool   `json:"finished,omitempty" jsonschema:"whether the run reported ok or failed; absent when no event was found"`
	RunningFor string  `json:"running_for,omitempty" jsonschema:"time since the run started, when it has not finished"`
}

func (m *monitor) toolRun(ctx context.Context, req *mcp.CallToolRequest, in runIn) (*mcp.CallToolResult, runOut, error) {
	if in.RunID == "" {
		return nil, runOut{}, fmt.Errorf("run_id is required")
	}
	_, recent := m.events.snapshot()
	out := runOut{RunID: in.RunID, Events: []Event{}}
	var started time.Time
	finished := false
	for _, e := range recent {
		if e.RunID != in.RunID {
			continue
		}
		if e.finished() {
			finished = true
		} else if started.IsZero() {
			started = e.Time
		}
		e.Time = e.Time.UTC()
		out.Events = append(out.Events, e)
	}
	if len(out.Events) > 0 {
		out.Finished = &finished
		if !finished && !started.IsZero() {
			out.RunningFor = fmtDur(time.Since(started))
		}
	}
	return nil, out, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// parseSince reads a look-back duration: def when empty, at most limit.
func parseSince(s string, def, limit time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > limit {
		return 0, fmt.Errorf("since must be a duration such as 90m or 6h, at most %s", shortDur(limit))
	}
	return d, nil
}
