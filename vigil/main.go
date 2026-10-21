// vigil watches a server and the applications on it: disk space, CPU, memory
// and load are sampled every few seconds, and applications report their
// downloads, ingestions and other jobs as events over HTTP. It serves a
// dashboard and a JSON API, and posts alerts to a webhook (Slack, Mattermost,
// Discord or anything that accepts JSON).
//
// Usage:
//
//	go run . -disks / -data ./data          # then open http://127.0.0.1:8090
//	./vigil -listen :8090 -disks /,/var/lib/garage -auth admin:secret -token s3cret \
//	    -expect url-to-bucket/download=24h -webhook https://hooks.slack.com/...
//
// Applications publish events to relay (-relay, see relay.go) as
// "<app>.<kind>" with type "started", "ok" or "failed", or report them
// directly with POST /api/events (see client/ for a Go
// client, or README.md for curl):
//
//	{"app":"url-to-bucket","kind":"download","status":"started","run_id":"8f3a...","name":"https://..."}
//	{"app":"url-to-bucket","kind":"download","status":"ok","run_id":"8f3a...","name":"https://...","bytes":123,"duration_ms":4500}
//	{"app":"url-to-bucket","kind":"download","status":"failed","error":"GET https://...: 503 Service Unavailable"}
//
// status is "started" (optional), then "ok" or "failed"; error holds the
// failure detail as text. Only "ok" and "failed" events count as runs. An
// optional run_id links a "started" event to its result.
package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

const history = 24 * time.Hour // how much sample history is kept in memory

type config struct {
	listen    string
	dataDir   string
	auth      string // user:pass for the dashboard and read APIs
	token     string // bearer token applications use to post events
	mcpToken  string // bearer token for /mcp; /mcp is not served without it
	webhook   string
	disks     []string
	interval  time.Duration
	retention time.Duration
	recent    time.Duration // how many events are kept in memory, for the MCP tools

	diskWarn float64
	cpuWarn  float64
	memWarn  float64
	sustain  time.Duration // how long CPU/memory must stay high before alerting

	expect map[string]time.Duration // app/kind -> longest allowed gap between successes

	relay      string // relay URL to read application events from
	relayToken string
}

type monitor struct {
	cfg     config
	host    string
	started time.Time
	events  *eventStore
	alerts  *alerter

	mu      sync.Mutex
	samples []Sample
	disks   []Disk
	prevCPU cpuTimes
	havePrv bool
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var cfg config
	cfg.expect = map[string]time.Duration{}
	disks := flag.String("disks", "/", "comma-separated paths whose filesystems to watch")
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:8090", "address to serve on")
	flag.StringVar(&cfg.dataDir, "data", envOr("STATE_DIRECTORY", "./data"), "directory for the event log")
	flag.StringVar(&cfg.auth, "auth", os.Getenv("VIGIL_AUTH"), "require HTTP basic auth for the dashboard and read APIs, as user:pass")
	flag.StringVar(&cfg.token, "token", os.Getenv("VIGIL_TOKEN"), "require this bearer token to post events")
	flag.StringVar(&cfg.mcpToken, "mcp-token", os.Getenv("VIGIL_MCP_TOKEN"), "serve MCP tools at /mcp, behind this bearer token (empty: don't serve /mcp)")
	flag.StringVar(&cfg.webhook, "webhook", os.Getenv("VIGIL_WEBHOOK"), "URL to POST alerts to (JSON)")
	flag.DurationVar(&cfg.interval, "interval", 15*time.Second, "how often to sample the system")
	flag.DurationVar(&cfg.retention, "retention", 90*24*time.Hour, "how long to keep events")
	flag.DurationVar(&cfg.recent, "recent", 7*24*time.Hour, "how long to keep events in memory, where the MCP tools can see them (at least 24h)")
	flag.Float64Var(&cfg.diskWarn, "disk-warn", 85, "alert when a disk is fuller than this many percent")
	flag.Float64Var(&cfg.cpuWarn, "cpu-warn", 90, "alert when CPU use stays above this many percent")
	flag.Float64Var(&cfg.memWarn, "mem-warn", 90, "alert when memory use stays above this many percent")
	flag.StringVar(&cfg.relay, "relay", os.Getenv("RELAY_URL"), "read application events from this relay (empty: don't)")
	flag.StringVar(&cfg.relayToken, "relay-token", os.Getenv("RELAY_TOKEN"), "bearer token for relay")
	flag.DurationVar(&cfg.sustain, "sustain", 5*time.Minute, "how long CPU or memory must stay high before alerting")
	flag.Func("expect", "alert when app/kind has no successful event for a duration, as app/kind=24h (repeatable)", func(s string) error {
		k, v, ok := strings.Cut(s, "=")
		d, err := time.ParseDuration(v)
		app, kind, ok2 := strings.Cut(k, "/")
		if !ok || !ok2 || err != nil || d <= 0 || !nameRE.MatchString(app) || !nameRE.MatchString(kind) {
			return fmt.Errorf("want app/kind=duration, e.g. url-to-bucket/download=24h")
		}
		cfg.expect[k] = d
		return nil
	})
	flag.Parse()
	for _, d := range strings.Split(*disks, ",") {
		if d = strings.TrimSpace(d); d != "" {
			cfg.disks = append(cfg.disks, d)
		}
	}
	if cfg.interval < time.Second {
		log.Fatal("-interval must be at least 1s")
	}
	if cfg.recent < 24*time.Hour {
		log.Fatal("-recent must be at least 24h: the dashboard shows the last day")
	}

	events, err := openEventStore(cfg.dataDir, cfg.retention, cfg.recent)
	if err != nil {
		log.Fatalf("event log: %v", err)
	}
	host, _ := os.Hostname()
	m := &monitor{cfg: cfg, host: host, started: time.Now(), events: events, alerts: newAlerter(cfg.webhook)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go m.run(ctx)
	if cfg.relay != "" {
		rc := &relayConsumer{url: strings.TrimRight(cfg.relay, "/"), token: cfg.relayToken, name: "vigil",
			http: &http.Client{Timeout: 45 * time.Second}}
		go rc.run(ctx, m)
	}

	srv := &http.Server{Addr: cfg.listen, Handler: m.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("vigil listening on http://%s (disks: %s, data: %s)", cfg.listen, strings.Join(cfg.disks, ", "), cfg.dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// --- sampling and checks ------------------------------------------------------

func (m *monitor) run(ctx context.Context) {
	m.sample()
	m.check()
	t := time.NewTicker(m.cfg.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sample()
			m.check()
		}
	}
}

func (m *monitor) sample() {
	s := Sample{Time: time.Now(), CPU: -1, Mem: readMem(), Load1: readLoad()}
	cur, ok := readCPU()
	disks := make([]Disk, len(m.cfg.disks))
	for i, p := range m.cfg.disks {
		disks[i] = diskUsage(p)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if ok && m.havePrv {
		s.CPU = cpuPercent(m.prevCPU, cur)
	}
	m.prevCPU, m.havePrv = cur, ok
	m.disks = disks
	if !ok && m.samples == nil {
		log.Printf("CPU and memory are not available on %s (no /proc); watching disks only", runtime.GOOS)
	}
	m.samples = append(m.samples, s)
	if n := int(history / m.cfg.interval); len(m.samples) > n+n/10 {
		m.samples = append([]Sample(nil), m.samples[len(m.samples)-n:]...)
	}
}

// sustained returns the average of f over the last d of samples, or false if
// there is not yet a full window of data.
func (m *monitor) sustained(d time.Duration, f func(Sample) float64) (float64, bool) {
	cutoff := time.Now().Add(-d)
	if len(m.samples) == 0 || m.samples[0].Time.After(cutoff) {
		return 0, false
	}
	sum, n := 0.0, 0
	for i := len(m.samples) - 1; i >= 0 && m.samples[i].Time.After(cutoff); i-- {
		if v := f(m.samples[i]); v >= 0 {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

func (m *monitor) check() {
	m.mu.Lock()
	disks := m.disks
	cpu, cpuOK := m.sustained(m.cfg.sustain, func(s Sample) float64 { return s.CPU })
	mem, memOK := m.sustained(m.cfg.sustain, func(s Sample) float64 { return s.Mem })
	m.mu.Unlock()

	for _, d := range disks {
		key := "disk:" + d.Path
		if d.Err != "" {
			m.alerts.set(key, true, fmt.Sprintf("cannot read disk %s: %s", d.Path, d.Err))
			continue
		}
		m.alerts.set(key, d.Pct >= m.cfg.diskWarn,
			fmt.Sprintf("disk %s is %.0f%% full (%s free of %s)", d.Path, d.Pct, fmtBytes(d.Free), fmtBytes(d.Total)))
	}
	if cpuOK {
		m.alerts.set("cpu", cpu >= m.cfg.cpuWarn,
			fmt.Sprintf("CPU averaged %.0f%% over the last %s", cpu, m.cfg.sustain))
	}
	if memOK {
		m.alerts.set("memory", mem >= m.cfg.memWarn,
			fmt.Sprintf("memory averaged %.0f%% over the last %s", mem, m.cfg.sustain))
	}
	for key, maxAge := range m.cfg.expect {
		last := m.events.lastSuccess(key)
		var stale bool
		var msg string
		if last == nil {
			stale = time.Since(m.started) > maxAge
			msg = fmt.Sprintf("no successful %s since vigil started %s ago (expected every %s)", key, fmtDur(time.Since(m.started)), shortDur(maxAge))
		} else {
			stale = time.Since(*last) > maxAge
			msg = fmt.Sprintf("no successful %s for %s (expected every %s)", key, fmtDur(time.Since(*last)), shortDur(maxAge))
		}
		m.alerts.set("stale:"+key, stale, msg)
	}
}

// --- HTTP ---------------------------------------------------------------------

func (m *monitor) routes() http.Handler {
	mux := http.NewServeMux()
	read := func(h http.HandlerFunc) http.Handler { return basicAuth(m.cfg.auth, h) }
	mux.Handle("GET /{$}", read(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	}))
	mux.Handle("GET /api/status", read(m.status))
	mux.HandleFunc("POST /api/events", m.postEvent)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	if m.cfg.mcpToken != "" {
		mux.Handle("/mcp", bearerAuth(m.cfg.mcpToken, m.mcpHandler()))
	}
	return mux
}

func basicAuth(auth string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != "" {
			user, pass, _ := r.BasicAuth()
			if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(auth)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="vigil"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (m *monitor) postEvent(w http.ResponseWriter, r *http.Request) {
	if m.cfg.token != "" {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(m.cfg.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	// Requiring JSON means a web page elsewhere can't post events without a
	// CORS preflight, which is never granted.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var e Event
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := e.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.RelayID = 0
	if err := m.record(e); err != nil {
		log.Printf("event log: %v", err)
		http.Error(w, "could not store event", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// record stores a validated event and raises an alert if it is a failure.
func (m *monitor) record(e Event) error {
	if err := m.events.add(e); err != nil {
		return err
	}
	if e.failed() {
		msg := fmt.Sprintf("%s failed: %s", e.key(), summary(e.Error))
		if e.Name != "" {
			msg += " (" + e.Name + ")"
		}
		m.alerts.notice("fail:"+e.key(), msg, 15*time.Minute)
	}
	return nil
}

type statView struct {
	Stat
	Count24    int64 `json:"count_24h"`
	Failures24 int64 `json:"failures_24h"`
	Bytes24    int64 `json:"bytes_24h"`
	Items24    int64 `json:"items_24h"`
	// Failures over the events held in memory (-recent), for the MCP tools.
	FailuresRecent int64  `json:"failures_recent"`
	Expect         string `json:"expect,omitempty"`
}

type hourBucket struct {
	Time     time.Time `json:"t"`
	Bytes    int64     `json:"bytes"`
	Count    int64     `json:"count"`
	Failures int64     `json:"failures"`
}

// jobs returns one row per app/kind, sorted, with the totals of the last 24
// hours and the failures of the whole window filled in from recent. Apps that are expected but have never
// reported still get a row.
func (m *monitor) jobs(stats []Stat, recent []Event) []*statView {
	views := map[string]*statView{}
	out := make([]*statView, len(stats))
	for i, st := range stats {
		v := &statView{Stat: st}
		if d, ok := m.cfg.expect[st.App+"/"+st.Kind]; ok {
			v.Expect = shortDur(d)
		}
		views[st.App+"/"+st.Kind] = v
		out[i] = v
	}
	for key, d := range m.cfg.expect {
		if views[key] == nil {
			app, kind, _ := strings.Cut(key, "/")
			v := &statView{Stat: Stat{App: app, Kind: kind}, Expect: shortDur(d)}
			views[key] = v
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].App+"/"+out[i].Kind < out[j].App+"/"+out[j].Kind })

	day := time.Now().Add(-24 * time.Hour)
	for _, e := range recent {
		if !e.finished() {
			continue
		}
		v := views[e.key()]
		if e.failed() {
			v.FailuresRecent++
		}
		if e.Time.Before(day) {
			continue
		}
		v.Count24++
		v.Bytes24 += e.Bytes
		v.Items24 += e.Items
		if e.failed() {
			v.Failures24++
		}
	}
	return out
}

func (m *monitor) status(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	samples := downsample(m.samples, 720)
	disks := m.disks
	m.mu.Unlock()
	stats, recent := m.events.snapshot()
	out := m.jobs(stats, recent)

	// 24 hourly buckets ending with the current hour.
	now := time.Now()
	first := now.Truncate(time.Hour).Add(-23 * time.Hour)
	hours := make([]hourBucket, 24)
	for i := range hours {
		hours[i].Time = first.Add(time.Duration(i) * time.Hour)
	}
	for _, e := range recent {
		if !e.finished() {
			continue
		}
		if i := int(e.Time.Sub(first) / time.Hour); i >= 0 && i < 24 {
			hours[i].Count++
			hours[i].Bytes += e.Bytes
			if e.failed() {
				hours[i].Failures++
			}
		}
	}

	// The latest events of the last 24 hours; recent may go further back.
	day := sort.Search(len(recent), func(i int) bool { return !recent[i].Time.Before(now.Add(-24 * time.Hour)) })
	last := recent[max(day, len(recent)-100):]
	latest := make([]Event, len(last))
	for i, e := range last {
		latest[len(last)-1-i] = e
	}

	writeJSON(w, map[string]any{
		"host":       m.host,
		"now":        now,
		"started":    m.started,
		"interval":   m.cfg.interval.String(),
		"thresholds": map[string]float64{"disk": m.cfg.diskWarn, "cpu": m.cfg.cpuWarn, "mem": m.cfg.memWarn},
		"samples":    samples,
		"disks":      disks,
		"stats":      out,
		"hours":      hours,
		"events":     latest,
		"alerts":     m.alerts.list(),
	})
}

// downsample averages samples into at most n points.
func downsample(s []Sample, n int) []Sample {
	if len(s) <= n {
		return append([]Sample(nil), s...)
	}
	out := make([]Sample, 0, n)
	step := float64(len(s)) / float64(n)
	for i := 0; i < n; i++ {
		chunk := s[int(float64(i)*step):int(float64(i+1)*step)]
		avg := Sample{Time: chunk[len(chunk)-1].Time}
		for _, f := range []struct {
			dst *float64
			get func(Sample) float64
		}{{&avg.CPU, func(x Sample) float64 { return x.CPU }}, {&avg.Mem, func(x Sample) float64 { return x.Mem }}, {&avg.Load1, func(x Sample) float64 { return x.Load1 }}} {
			sum, k := 0.0, 0
			for _, x := range chunk {
				if v := f.get(x); v >= 0 {
					sum += v
					k++
				}
			}
			*f.dst = -1
			if k > 0 {
				*f.dst = sum / float64(k)
			}
		}
		out = append(out, avg)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// summary shortens multi-line error detail for alerts to its most telling
// line: the first, or the last for a Python traceback, where the error is at
// the end. The full text stays in the event log and on the dashboard.
func summary(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	s, cut := lines[0], len(lines) > 1
	if strings.HasPrefix(s, "Traceback (most recent call last)") {
		s = strings.TrimSpace(lines[len(lines)-1])
	}
	if len(s) > 300 {
		s, cut = s[:300], true
	}
	if cut {
		s += " …"
	}
	return s
}

func fmtBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// shortDur formats a configured duration without zero units: 1h, not 1h0m0s.
func shortDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	d = d.Round(time.Minute)
	if h := int(d.Hours()); h >= 48 {
		return fmt.Sprintf("%dd", h/24)
	} else if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
