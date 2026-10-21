package main

// "url-to-bucket serve" takes download requests from relay instead of the
// command line. A request is an event on the same topic as the results:
//
//	relay pub url-to-bucket.download requested -run-id abc \
//	  -data '{"url":"https://…/file.iso","bucket":"home","key":"isos/"}'
//	relay wait -run-id abc -type ok,failed
//
// data takes url and bucket (required), key and content_type. key works as on
// the command line. The download publishes "started" and then "ok" or
// "failed" with the request's run_id (or "req-<request id>" if it has none)
// and cause set to the request's ID. An invalid request gets a "failed" only.
//
// The service runs up to -workers requests at a time, starting the next one
// as soon as a worker is free. Its relay consumer "url-to-bucket" is acked up
// to the newest request with all earlier ones done, so a request whose
// download was cut short by a restart runs again.
//
// It also serves a web page on -listen to make requests and follow their
// progress (ui.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	relay "relay/client"
)

func serveMain(args []string) {
	fs := flag.NewFlagSet("url-to-bucket serve", flag.ExitOnError)
	endpoint := fs.String("endpoint-url", os.Getenv("AWS_ENDPOINT_URL"), "S3 endpoint (default: $AWS_ENDPOINT_URL)")
	profile := fs.String("profile", "", "AWS profile name from ~/.aws")
	workers := fs.Int("workers", 2, "downloads run at the same time")
	name := fs.String("consumer", "url-to-bucket", "relay consumer name")
	listen := fs.String("listen", "127.0.0.1:8093", "address of the web page; empty for none")
	auth := fs.String("auth", os.Getenv("URL_TO_BUCKET_UI_AUTH"), "require HTTP basic auth on the page, as user:pass")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Download URLs requested through relay into S3 buckets.\n\nusage: url-to-bucket serve [flags]\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() > 0 || *workers < 1 {
		fs.Usage()
		os.Exit(2)
	}
	log.SetFlags(0) // journald adds the time

	bus := relay.FromEnv()
	if bus == nil {
		log.Fatalf("relay is not configured: set RELAY_URL and RELAY_TOKEN, or install relay (%s)", relay.EnvFile)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := newS3Client(ctx, *endpoint, *profile)
	if err != nil {
		log.Fatal(err)
	}

	c := &consumer{url: bus.URL, token: bus.Token, name: *name, http: &http.Client{Timeout: 45 * time.Second}}
	page := &ui{bus: bus, relay: c, s3: client, active: map[string]*progress{}}
	if *listen != "" {
		go page.serve(ctx, *listen, *auth)
	}
	c.run(ctx, *workers, func(ctx context.Context, ev requestEvent) {
		j, err := ev.job()
		if err != nil {
			log.Printf("request %d: invalid: %v", ev.ID, err)
			reject(bus, ev, err)
			return
		}
		log.Printf("request %d: %s -> s3://%s/%s", ev.ID, j.URL, j.Bucket, j.Key)
		p := &progress{}
		defer page.track(j.RunID, p)()
		if err := runJob(ctx, bus, client, j, p); err != nil {
			log.Printf("request %d: failed: %v", ev.ID, err)
			return
		}
		log.Printf("request %d: ok", ev.ID)
	})
}

// requestEvent is the part of a relay "requested" event the service uses.
type requestEvent struct {
	ID    uint64          `json:"id"`
	RunID string          `json:"run_id"`
	Data  json.RawMessage `json:"data"`
}

// job validates the request and returns the download it asks for.
func (ev requestEvent) job() (job, error) {
	var r struct {
		URL         string `json:"url"`
		Bucket      string `json:"bucket"`
		Key         string `json:"key"`
		ContentType string `json:"content_type"`
	}
	if len(ev.Data) == 0 || json.Unmarshal(ev.Data, &r) != nil {
		return job{}, errors.New(`data must be an object like {"url":"…","bucket":"…"}`)
	}
	if u, err := url.Parse(r.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return job{}, fmt.Errorf("url must be an http or https URL, got %q", r.URL)
	}
	if r.Bucket == "" {
		return job{}, errors.New("bucket is required")
	}
	runID := ev.RunID
	if runID == "" {
		// Fixed per request, so a rerun after a restart keeps the run ID.
		runID = fmt.Sprintf("req-%d", ev.ID)
	}
	return job{URL: r.URL, Bucket: r.Bucket, Key: resolveKey(r.URL, r.Key), ContentType: r.ContentType,
		RunID: runID, Cause: ev.ID}, nil
}

// reject publishes "failed" for a request that can't be run.
func reject(bus *relay.Client, ev requestEvent, reason error) {
	runID := ev.RunID
	if runID == "" {
		runID = fmt.Sprintf("req-%d", ev.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := bus.Publish(ctx, relay.Event{Topic: topic, Type: "failed", RunID: runID, Cause: ev.ID,
		Key:  fmt.Sprintf("req-%d-failed", ev.ID),
		Data: map[string]any{"name": fmt.Sprintf("invalid request %d", ev.ID), "error": reason.Error()}})
	if err != nil {
		log.Printf("warning: could not publish to relay: %v", err)
	}
}

// consumer reads "requested" events through a named relay consumer.
type consumer struct {
	url, token, name string
	http             *http.Client
}

func (c *consumer) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
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

// run creates the consumer (a new one starts at relay's newest event; an
// existing one keeps its position), then runs handle on each request after
// the consumer's cursor, up to workers at a time. A request is fetched as
// soon as a worker is free, so a long download doesn't hold up the others.
// The cursor is acked up to the newest request whose predecessors are all
// done. When ctx ends, run waits for the running handlers and returns; those
// cut short are not acked. relay errors are logged and retried with a
// growing delay.
func (c *consumer) run(ctx context.Context, workers int, handle func(context.Context, requestEvent)) {
	// retry logs err and waits *backoff, which grows for the next time.
	retry := func(err error, backoff *time.Duration) {
		log.Printf("relay: %v (retrying in %s)", err, *backoff)
		select {
		case <-ctx.Done():
		case <-time.After(*backoff):
		}
		*backoff = min(*backoff*2, time.Minute)
	}
	backoff := time.Second
	var fetched uint64 // newest request taken
	for ctx.Err() == nil {
		var view struct {
			Cursor uint64 `json:"cursor"`
		}
		err := c.do(ctx, http.MethodPut, "/v1/consumers/"+c.name,
			map[string]string{"topic": topic, "type": "requested"}, &view)
		if err == nil {
			fetched = view.Cursor
			break
		}
		retry(err, &backoff)
	}
	log.Printf("relay: reading requests from %s as consumer %q, %d at a time", c.url, c.name, workers)

	var (
		mu      sync.Mutex
		pending []uint64 // requests taken and not yet acked, oldest first
		done    = map[uint64]bool{}
		ackMu   sync.Mutex // keeps acks in order
		wg      sync.WaitGroup
		slots   = make(chan struct{}, workers)
	)
	finish := func(id uint64) {
		mu.Lock()
		done[id] = true
		var ack uint64
		for len(pending) > 0 && done[pending[0]] {
			ack = pending[0]
			delete(done, ack)
			pending = pending[1:]
		}
		ackMu.Lock() // taken before mu is released, so acks go out in order
		mu.Unlock()
		defer ackMu.Unlock()
		// Retry until it lands, or the requests would run again on restart.
		ackBackoff := time.Second
		for ack != 0 && ctx.Err() == nil {
			err := c.do(ctx, http.MethodPost, "/v1/consumers/"+c.name+"/ack", map[string]uint64{"id": ack}, nil)
			if err == nil {
				return
			}
			retry(err, &ackBackoff)
		}
	}

	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		var resp struct {
			Events []requestEvent `json:"events"`
		}
		path := fmt.Sprintf("/v1/events?topic=%s&type=requested&after=%d&limit=1&wait=30s", topic, fetched)
		if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			<-slots
			if ctx.Err() == nil {
				retry(err, &backoff)
			}
			continue
		}
		backoff = time.Second
		if len(resp.Events) == 0 {
			<-slots
			continue
		}
		ev := resp.Events[0]
		fetched = ev.ID
		mu.Lock()
		pending = append(pending, ev.ID)
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			handle(ctx, ev)
			if ctx.Err() == nil {
				finish(ev.ID)
			}
		}()
	}
	wg.Wait()
}
