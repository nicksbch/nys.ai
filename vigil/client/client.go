// Package client reports events (downloads, ingestions, any job) to vigil.
//
// It never gets in the application's way: a nil *Client, from FromEnv when
// neither VIGIL_URL nor /etc/vigil-client.env is set, turns every call into a
// no-op, and a monitor that is
// down only costs a short timeout. Errors are returned from the synchronous
// calls for logging, never to fail the job itself.
//
//	mon := client.FromEnv("my-app") // VIGIL_URL, VIGIL_TOKEN or /etc/vigil-client.env
//
//	op := mon.Track("download", url)
//	op.Start()                      // optional: report that it began
//	body = op.Reader(body)          // counts bytes as they are read
//	err := doTheWork(body)
//	op.Done(err)                    // in a server: reports in the background
//	op.DoneWait(ctx, err)           // in a CLI that is about to exit
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Status values.
const (
	Started = "started"
	OK      = "ok"
	Failed  = "failed"
)

// Event mirrors the server's event. Status is Started when an operation
// begins (optional), then OK or Failed when it ends; for a failure,
// Error carries the detail as text. If Status is empty the server sets it
// from Error.
type Event struct {
	Time       time.Time `json:"time"`
	App        string    `json:"app"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status,omitempty"`
	RunID      string    `json:"run_id,omitempty"`
	Name       string    `json:"name,omitempty"`
	Bytes      int64     `json:"bytes,omitempty"`
	Items      int64     `json:"items,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type Client struct {
	URL   string // base URL of vigil, e.g. http://127.0.0.1:8090
	Token string // bearer token, if vigil requires one
	App   string // default App for events
	HTTP  *http.Client
}

// EnvFile is where vigil's install.sh writes VIGIL_URL and VIGIL_TOKEN for
// applications on the same server.
const EnvFile = "/etc/vigil-client.env"

// FromEnv returns a client configured from VIGIL_URL and VIGIL_TOKEN. If
// VIGIL_URL is not in the environment (cron, "ssh host cmd", shells that don't
// read ~/.profile), both are read from EnvFile instead. It returns nil (a
// no-op client) if neither has a URL.
func FromEnv(app string) *Client {
	u, token := os.Getenv("VIGIL_URL"), os.Getenv("VIGIL_TOKEN")
	if u == "" {
		vars := readEnvFile(EnvFile)
		u, token = vars["VIGIL_URL"], vars["VIGIL_TOKEN"]
	}
	if u == "" {
		return nil
	}
	return &Client{
		URL:   strings.TrimRight(u, "/"),
		Token: token,
		App:   app,
		HTTP:  &http.Client{Timeout: 3 * time.Second},
	}
}

// readEnvFile parses KEY=VALUE lines; a missing file gives an empty map.
func readEnvFile(path string) map[string]string {
	vars := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return vars
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && !strings.HasPrefix(k, "#") {
			vars[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return vars
}

// Send posts one event and waits for vigil to accept it.
func (c *Client) Send(ctx context.Context, e Event) error {
	if c == nil {
		return nil
	}
	if e.App == "" {
		e.App = c.App
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("vigil: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("vigil: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Go posts an event in the background and ignores the outcome. Use it in
// long-running servers; a CLI should use Send so the event isn't lost at exit.
func (c *Client) Go(e Event) {
	if c == nil {
		return
	}
	go c.Send(context.Background(), e)
}

// Op times one operation and counts the bytes and items it handles.
type Op struct {
	c     *Client
	ev    Event
	start time.Time
	bytes atomic.Int64
	items atomic.Int64
}

// Track starts an operation of the given kind ("download", "ingest", ...);
// name says what it works on (a URL, file, table...). Its events share a
// random run ID, which links the "started" event to the result.
func (c *Client) Track(kind, name string) *Op {
	return &Op{c: c, ev: Event{Kind: kind, Name: name, RunID: newRunID()}, start: time.Now()}
}

// RunID returns the ID shared by the operation's events.
func (o *Op) RunID() string { return o.ev.RunID }

func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// AddBytes and AddItems add to the operation's counters. They are safe to
// call from several goroutines.
func (o *Op) AddBytes(n int64) { o.bytes.Add(n) }
func (o *Op) AddItems(n int64) { o.items.Add(n) }

// Reader wraps r so every byte read from it is counted.
func (o *Op) Reader(r io.Reader) io.Reader { return &countingReader{r: r, op: o} }

// Writer wraps w so every byte written to it is counted.
func (o *Op) Writer(w io.Writer) io.Writer { return &countingWriter{w: w, op: o} }

func (o *Op) event(err error) Event {
	e := o.ev
	e.Time = time.Now()
	e.Bytes = o.bytes.Load()
	e.Items = o.items.Load()
	e.DurationMS = time.Since(o.start).Milliseconds()
	e.Status = OK
	if err != nil {
		e.Status, e.Error = Failed, err.Error()
	}
	return e
}

func (o *Op) startEvent() Event {
	return Event{Time: o.start, Kind: o.ev.Kind, Name: o.ev.Name, RunID: o.ev.RunID, Status: Started}
}

// Start reports in the background that the operation has begun. It is
// optional; only the event from Done counts as a run.
func (o *Op) Start() { o.c.Go(o.startEvent()) }

// StartWait reports that the operation has begun and waits for vigil to
// accept it, so it is recorded before the operation's own result.
func (o *Op) StartWait(ctx context.Context) error { return o.c.Send(ctx, o.startEvent()) }

// Done reports the operation in the background. err != nil marks it failed,
// with err.Error() as the detail.
func (o *Op) Done(err error) { o.c.Go(o.event(err)) }

// DoneWait reports the operation and waits for vigil to accept it.
func (o *Op) DoneWait(ctx context.Context, err error) error { return o.c.Send(ctx, o.event(err)) }

type countingReader struct {
	r  io.Reader
	op *Op
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.op.bytes.Add(int64(n))
	return n, err
}

type countingWriter struct {
	w  io.Writer
	op *Op
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.op.bytes.Add(int64(n))
	return n, err
}
