package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"relay/client"
)

// Exit codes, part of the CLI's contract.
const (
	exitOK      = 0
	exitFailed  = 1 // wait matched a "failed" event
	exitErr     = 2 // usage error, or the server could not be reached or said no
	exitNothing = 3 // wait timed out, or next found nothing
	exitUsage   = exitErr
)

// listenBackoff is the first delay before listen retries a failed command.
var listenBackoff = 5 * time.Second

const maxBackoff = 5 * time.Minute

type cli struct {
	ctx    context.Context
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// fail prints err as JSON on stderr and returns exitErr.
func (c *cli) fail(err error) int {
	json.NewEncoder(c.stderr).Encode(map[string]string{"error": err.Error()})
	return exitErr
}

func (c *cli) print(v any) { json.NewEncoder(c.stdout).Encode(v) }

func (c *cli) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("relay "+name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	return fs
}

// parse parses flags placed anywhere among the positionals. Everything after
// "--" is returned untouched as rest.
func parse(fs *flag.FlagSet, args []string, npos int) (pos, rest []string, ok bool) {
	for i, a := range args {
		if a == "--" {
			args, rest = args[:i], args[i+1:]
			break
		}
	}
	for {
		if fs.Parse(args) != nil {
			return nil, nil, false
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != npos {
		fmt.Fprintf(fs.Output(), "%s: want %d argument(s), got %d (see relay help)\n", fs.Name(), npos, len(pos))
		return nil, nil, false
	}
	return pos, rest, true
}

// call sends one request to the server and decodes the JSON reply into out.
// wait is how long the server may hold the request (long-poll).
func (c *cli) call(method, path string, q url.Values, body, out any, wait time.Duration) error {
	cl := client.FromEnv()
	if cl == nil {
		return errors.New("RELAY_URL is not set and " + client.EnvFile + " does not exist")
	}
	ctx, cancel := context.WithTimeout(c.ctx, wait+10*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := cl.URL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cl.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cl.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = string(bytes.TrimSpace(b))
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func filterQuery(topic, typ, runID string) url.Values {
	q := url.Values{}
	for k, v := range map[string]string{"topic": topic, "type": typ, "run_id": runID} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

func (c *cli) pub(args []string) int {
	fs := c.flags("pub")
	runID := fs.String("run-id", "", "run ID shared by the events of one run")
	cause := fs.Uint64("cause", 0, "ID of the event that led to this one")
	key := fs.String("key", "", "idempotency key")
	data := fs.String("data", "", "JSON object, @file, or - for stdin")
	pos, _, ok := parse(fs, args, 2)
	if !ok {
		return exitUsage
	}
	var raw []byte
	switch {
	case *data == "-":
		b, err := io.ReadAll(c.stdin)
		if err != nil {
			return c.fail(err)
		}
		raw = b
	case len(*data) > 0 && (*data)[0] == '@':
		b, err := os.ReadFile((*data)[1:])
		if err != nil {
			return c.fail(err)
		}
		raw = b
	default:
		raw = []byte(*data)
	}
	body := map[string]any{"topic": pos[0], "type": pos[1], "run_id": *runID, "cause": *cause, "key": *key}
	if len(bytes.TrimSpace(raw)) > 0 {
		if !json.Valid(raw) {
			return c.fail(errors.New("-data is not valid JSON"))
		}
		body["data"] = json.RawMessage(raw)
	}
	var e Event
	if err := c.call("POST", "/v1/events", nil, body, &e, 0); err != nil {
		return c.fail(err)
	}
	c.print(e)
	return exitOK
}

func (c *cli) tail(args []string) int {
	fs := c.flags("tail")
	topic := fs.String("topic", "", "topic filter: a.b, a.* or *, comma-separated")
	typ := fs.String("type", "", "types, comma-separated")
	runID := fs.String("run-id", "", "only this run")
	after := fs.Int64("after", -1, "print every event after this ID instead of the last -n")
	n := fs.Int("n", 20, "how many recent events to print")
	follow := fs.Bool("f", false, "keep printing new events as they arrive")
	if _, _, ok := parse(fs, args, 0); !ok {
		return exitUsage
	}
	q := filterQuery(*topic, *typ, *runID)
	var resp eventsResponse
	var cursor uint64
	if *after < 0 {
		q.Set("last", strconv.Itoa(*n))
		if err := c.call("GET", "/v1/events", q, nil, &resp, 0); err != nil {
			return c.fail(err)
		}
		q.Del("last")
		for _, e := range resp.Events {
			c.print(e)
		}
		cursor = resp.Next
	} else {
		cursor = uint64(*after)
		q.Set("limit", strconv.Itoa(maxLimit))
		for {
			q.Set("after", strconv.FormatUint(cursor, 10))
			if err := c.call("GET", "/v1/events", q, nil, &resp, 0); err != nil {
				return c.fail(err)
			}
			for _, e := range resp.Events {
				c.print(e)
			}
			cursor = resp.Next
			if len(resp.Events) < maxLimit {
				break
			}
		}
	}
	if !*follow {
		return exitOK
	}
	q.Set("wait", maxWait.String())
	for c.ctx.Err() == nil {
		q.Set("after", strconv.FormatUint(cursor, 10))
		if err := c.call("GET", "/v1/events", q, nil, &resp, maxWait); err != nil {
			if c.ctx.Err() != nil {
				break
			}
			return c.fail(err)
		}
		for _, e := range resp.Events {
			c.print(e)
		}
		cursor = resp.Next
	}
	return exitOK
}

func (c *cli) wait(args []string) int {
	fs := c.flags("wait")
	topic := fs.String("topic", "", "topic filter: a.b, a.* or *, comma-separated")
	typ := fs.String("type", "", "types, comma-separated, e.g. ok,failed")
	runID := fs.String("run-id", "", "only this run (also looks back through its earlier events)")
	after := fs.Int64("after", -1, "only events after this ID (default: the newest, or 0 with -run-id)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait")
	if _, _, ok := parse(fs, args, 0); !ok {
		return exitUsage
	}
	q := filterQuery(*topic, *typ, *runID)
	q.Set("limit", "1")
	switch {
	case *after >= 0:
		q.Set("after", strconv.FormatInt(*after, 10))
	case *runID != "":
		q.Set("after", "0")
	}
	deadline := time.Now().Add(*timeout)
	for {
		w := min(time.Until(deadline), maxWait)
		if w < 0 {
			w = 0
		}
		q.Set("wait", w.String())
		var resp eventsResponse
		if err := c.call("GET", "/v1/events", q, nil, &resp, w); err != nil {
			if c.ctx.Err() != nil {
				return exitNothing
			}
			return c.fail(err)
		}
		if len(resp.Events) > 0 {
			e := resp.Events[0]
			c.print(e)
			if e.Type == "failed" {
				return exitFailed
			}
			return exitOK
		}
		q.Set("after", strconv.FormatUint(resp.Next, 10))
		if w == 0 || c.ctx.Err() != nil {
			return exitNothing
		}
	}
}

func (c *cli) topics(args []string) int {
	if _, _, ok := parse(c.flags("topics"), args, 0); !ok {
		return exitUsage
	}
	var ts []topicInfo
	if err := c.call("GET", "/v1/topics", nil, nil, &ts, 0); err != nil {
		return c.fail(err)
	}
	for _, t := range ts {
		c.print(t)
	}
	return exitOK
}

func (c *cli) consumer(args []string) int {
	fs := c.flags("consumer")
	topic := fs.String("topic", "*", "topic filter: a.b, a.* or *, comma-separated")
	typ := fs.String("type", "", "types, comma-separated")
	from := fs.String("from", "", "where to start: latest, earliest or an event ID (default: latest for a new consumer, unchanged for an existing one)")
	pos, _, ok := parse(fs, args, 1)
	if !ok {
		return exitUsage
	}
	body := map[string]any{"topic": *topic, "type": *typ}
	if *from != "" {
		body["from"] = *from
	}
	var v consumerView
	if err := c.call("PUT", "/v1/consumers/"+url.PathEscape(pos[0]), nil, body, &v, 0); err != nil {
		return c.fail(err)
	}
	c.print(v)
	return exitOK
}

func (c *cli) consumers(args []string) int {
	if _, _, ok := parse(c.flags("consumers"), args, 0); !ok {
		return exitUsage
	}
	var vs []consumerView
	if err := c.call("GET", "/v1/consumers", nil, nil, &vs, 0); err != nil {
		return c.fail(err)
	}
	for _, v := range vs {
		c.print(v)
	}
	return exitOK
}

// nextEvents long-polls a consumer until it has events or wait has passed.
func (c *cli) nextEvents(name string, n int, wait time.Duration) (eventsResponse, error) {
	q := url.Values{"limit": {strconv.Itoa(n)}}
	deadline := time.Now().Add(wait)
	for {
		w := max(min(time.Until(deadline), maxWait), 0)
		q.Set("wait", w.String())
		var resp eventsResponse
		err := c.call("GET", "/v1/consumers/"+url.PathEscape(name)+"/next", q, nil, &resp, w)
		if err != nil || len(resp.Events) > 0 || w == 0 || c.ctx.Err() != nil {
			return resp, err
		}
	}
}

func (c *cli) next(args []string) int {
	fs := c.flags("next")
	n := fs.Int("n", 1, "how many events to return")
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for an event")
	pos, _, ok := parse(fs, args, 1)
	if !ok {
		return exitUsage
	}
	resp, err := c.nextEvents(pos[0], *n, *wait)
	if err != nil {
		return c.fail(err)
	}
	for _, e := range resp.Events {
		c.print(e)
	}
	if len(resp.Events) == 0 {
		return exitNothing
	}
	return exitOK
}

func (c *cli) ack(args []string) int {
	pos, _, ok := parse(c.flags("ack"), args, 2)
	if !ok {
		return exitUsage
	}
	id, err := strconv.ParseUint(pos[1], 10, 64)
	if err != nil {
		return c.fail(fmt.Errorf("bad event ID %q", pos[1]))
	}
	var v consumerView
	if err := c.call("POST", "/v1/consumers/"+url.PathEscape(pos[0])+"/ack", nil, map[string]uint64{"id": id}, &v, 0); err != nil {
		return c.fail(err)
	}
	c.print(v)
	return exitOK
}

// sleep waits for d or until the CLI is interrupted, and reports which.
func (c *cli) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.ctx.Done():
		return false
	}
}

// listen runs a command for each of a consumer's events and acks the event
// when the command succeeds. A failing command is retried on the same event,
// with backoff, so nothing is skipped silently.
func (c *cli) listen(args []string) int {
	fs := c.flags("listen")
	wait := fs.Duration("wait", maxWait, "long-poll duration for each request")
	pos, cmdArgs, ok := parse(fs, args, 1)
	if !ok {
		return exitUsage
	}
	if len(cmdArgs) == 0 {
		fmt.Fprintln(c.stderr, "relay listen: give the command after --, e.g. relay listen indexer -- ./handle.sh")
		return exitUsage
	}
	name := pos[0]
	lg := slog.New(slog.NewJSONHandler(c.stderr, nil)).With("consumer", name)
	lg.Info("listening", "cmd", cmdArgs)

	for c.ctx.Err() == nil {
		resp, err := c.nextEvents(name, 1, *wait)
		if err != nil {
			if c.ctx.Err() != nil {
				break
			}
			lg.Error("reading events", "err", err.Error(), "retry_in", listenBackoff.String())
			c.sleep(listenBackoff)
			continue
		}
		if resp.Gap {
			lg.Warn("some events were removed by retention before this consumer read them")
		}
		if len(resp.Events) == 0 {
			continue
		}
		e := resp.Events[0]
		for delay := listenBackoff; ; delay = min(delay*2, maxBackoff) {
			start := time.Now()
			err := c.runCmd(e, cmdArgs)
			if c.ctx.Err() != nil {
				lg.Info("stopped", "id", e.ID)
				return exitOK
			}
			if err == nil {
				lg.Info("handled", "id", e.ID, "topic", e.Topic, "type", e.Type, "duration_ms", time.Since(start).Milliseconds())
				break
			}
			lg.Error("command failed", "id", e.ID, "topic", e.Topic, "type", e.Type, "err", err.Error(), "retry_in", delay.String())
			if !c.sleep(delay) {
				return exitOK
			}
		}
		for delay := listenBackoff; ; delay = min(delay*2, maxBackoff) {
			err := c.call("POST", "/v1/consumers/"+url.PathEscape(name)+"/ack", nil, map[string]uint64{"id": e.ID}, nil, 0)
			if err == nil {
				break
			}
			lg.Error("ack failed", "id", e.ID, "err", err.Error(), "retry_in", delay.String())
			if !c.sleep(delay) {
				return exitOK
			}
		}
	}
	lg.Info("stopped")
	return exitOK
}

// runCmd runs the listener's command for one event: the event JSON on stdin,
// its fields in the environment, output passed through. On interrupt the
// command gets SIGTERM and 10 s to finish.
func (c *cli) runCmd(e Event, args []string) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(c.ctx, args[0], args[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdin = bytes.NewReader(append(b, '\n'))
	cmd.Stdout, cmd.Stderr = c.stdout, c.stderr
	cmd.Env = append(os.Environ(),
		"RELAY_EVENT_ID="+strconv.FormatUint(e.ID, 10),
		"RELAY_TOPIC="+e.Topic,
		"RELAY_TYPE="+e.Type,
		"RELAY_RUN_ID="+e.RunID,
	)
	return cmd.Run()
}
