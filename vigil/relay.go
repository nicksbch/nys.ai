package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// relayConsumer reads application events from relay (../relay) through a
// named consumer, so apps publish once and vigil picks them up. An event's
// topic "<app>.<kind>" and type "started", "ok" or "failed" become vigil's
// app, kind and status; name, bytes, items, duration_ms and error come from
// its data. Delivery is at least once, so events whose relay ID vigil has
// already stored are skipped.
type relayConsumer struct {
	url, token, name string
	http             *http.Client
}

// relayEvent is the part of a relay event vigil uses.
type relayEvent struct {
	ID    uint64          `json:"id"`
	Time  time.Time       `json:"time"`
	Topic string          `json:"topic"`
	Type  string          `json:"type"`
	RunID string          `json:"run_id"`
	Data  json.RawMessage `json:"data"`
}

func (c *relayConsumer) do(ctx context.Context, method, path string, body, out any) error {
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
// existing one keeps its position), then stores and acks events until ctx
// ends. Errors are logged and retried with a growing delay.
func (c *relayConsumer) run(ctx context.Context, m *monitor) {
	backoff := time.Second
	retry := func(err error) {
		log.Printf("relay: %v (retrying in %s)", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	for ctx.Err() == nil {
		err := c.do(ctx, http.MethodPut, "/v1/consumers/"+c.name,
			map[string]string{"topic": "*", "type": "started,ok,failed"}, nil)
		if err == nil {
			break
		}
		retry(err)
	}
	log.Printf("relay: reading events from %s as consumer %q", c.url, c.name)
	for ctx.Err() == nil {
		var resp struct {
			Events []relayEvent `json:"events"`
			Gap    bool         `json:"gap"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/consumers/"+c.name+"/next?limit=100&wait=30s", nil, &resp); err != nil {
			if ctx.Err() == nil {
				retry(err)
			}
			continue
		}
		if resp.Gap {
			log.Printf("relay: some events expired before vigil read them")
		}
		if len(resp.Events) == 0 {
			continue
		}
		for _, re := range resp.Events {
			if re.ID <= m.events.lastRelayID() {
				continue
			}
			e, err := re.event()
			if err == nil {
				err = m.record(e)
			}
			if err != nil {
				log.Printf("relay: skipping event %d (%s %s): %v", re.ID, re.Topic, re.Type, err)
			}
		}
		last := resp.Events[len(resp.Events)-1].ID
		if err := c.do(ctx, http.MethodPost, "/v1/consumers/"+c.name+"/ack", map[string]uint64{"id": last}, nil); err != nil {
			retry(err)
			continue
		}
		backoff = time.Second
	}
}

// event converts a relay event into a validated vigil event.
func (re relayEvent) event() (Event, error) {
	app, kind, _ := strings.Cut(re.Topic, ".")
	e := Event{Time: re.Time, App: app, Kind: kind, Status: re.Type, RunID: re.RunID, RelayID: re.ID}
	if len(re.Data) > 0 {
		var d struct {
			Name       string `json:"name"`
			Bytes      int64  `json:"bytes"`
			Items      int64  `json:"items"`
			DurationMS int64  `json:"duration_ms"`
			Error      string `json:"error"`
		}
		// Fields of the wrong type are left out; the rest are still used.
		json.Unmarshal(re.Data, &d)
		e.Name, e.Bytes, e.Items, e.DurationMS, e.Error = d.Name, d.Bytes, d.Items, d.DurationMS, d.Error
	}
	if e.Status != statusFailed {
		e.Error = ""
	}
	return e, e.validate()
}
