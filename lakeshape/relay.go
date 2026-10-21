package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	relay "relay/client"
)

// consumerName is the relay consumer the service reads its triggers with.
const consumerName = "lakeshape"

// relayEvent is an event as relay's API returns it.
type relayEvent struct {
	ID    uint64         `json:"id"`
	Topic string         `json:"topic"`
	Type  string         `json:"type"`
	RunID string         `json:"run_id"`
	Data  map[string]any `json:"data"`
}

// relayAPI reads events from relay and moves the service's consumer. It uses
// the same RELAY_URL and RELAY_TOKEN (or /etc/relay-client.env) as publishing.
type relayAPI struct {
	url, token string
	http       *http.Client
}

func newRelayAPI(c *relay.Client) *relayAPI {
	if c == nil {
		return nil
	}
	return &relayAPI{url: c.URL, token: c.Token, http: &http.Client{Timeout: 90 * time.Second}}
}

func (r *relayAPI) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	u := r.url + path
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
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var msg struct{ Error string }
		if json.Unmarshal(b, &msg) != nil || msg.Error == "" {
			msg.Error = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("relay: %s %s: %s: %s", method, path, resp.Status, msg.Error)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// consumer creates or updates the service's consumer with this filter, and
// returns its cursor. A new consumer starts at the newest event.
func (r *relayAPI) consumer(ctx context.Context, topic, typ string) (uint64, error) {
	var v struct {
		Cursor uint64 `json:"cursor"`
	}
	err := r.call(ctx, "PUT", "/v1/consumers/"+consumerName, nil, map[string]string{"topic": topic, "type": typ}, &v)
	return v.Cursor, err
}

// events returns the events after id that match the filter, waiting up to
// wait for one, and the id to read after next.
func (r *relayAPI) events(ctx context.Context, after uint64, topic, typ, runID string, wait time.Duration) ([]relayEvent, uint64, error) {
	q := url.Values{"after": {fmt.Sprint(after)}, "limit": {"1000"}, "wait": {wait.String()}}
	if topic != "" {
		q.Set("topic", topic)
	}
	if typ != "" {
		q.Set("type", typ)
	}
	if runID != "" {
		q.Set("run_id", runID)
	}
	var resp struct {
		Events []relayEvent `json:"events"`
		Next   uint64       `json:"next"`
	}
	if err := r.call(ctx, "GET", "/v1/events", q, nil, &resp); err != nil {
		return nil, after, err
	}
	return resp.Events, max(resp.Next, after), nil
}

func (r *relayAPI) ack(ctx context.Context, id uint64) error {
	return r.call(ctx, "POST", "/v1/consumers/"+consumerName+"/ack", nil, map[string]uint64{"id": id}, nil)
}

// publisher sends events to relay in the order they happen, in the
// background, so a relay that is down never holds up a run.
type publisher struct {
	bus *relay.Client
	q   chan relay.Event
}

func newPublisher(bus *relay.Client) *publisher {
	p := &publisher{bus: bus, q: make(chan relay.Event, 1000)}
	if bus != nil {
		go func() {
			for e := range p.q {
				if _, err := bus.Publish(context.Background(), e); err != nil {
					log.Printf("relay: %v", err)
				}
			}
		}()
	}
	return p
}

// notify returns a runOptions.notify that publishes with run ID runID.
func (p *publisher) notify(runID string) func(topic, typ string, data map[string]any) {
	return func(topic, typ string, data map[string]any) {
		p.send(relay.Event{Topic: topic, Type: typ, RunID: runID, Data: data})
	}
}

func (p *publisher) send(e relay.Event) {
	if p.bus == nil {
		return
	}
	select {
	case p.q <- e:
	default:
		log.Printf("relay: queue full, dropping %s %s", e.Topic, e.Type)
	}
}

// flush waits up to d for queued events to be sent.
func (p *publisher) flush(d time.Duration) {
	deadline := time.Now().Add(d)
	for len(p.q) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}
