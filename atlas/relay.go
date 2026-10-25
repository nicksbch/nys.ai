package main

// What atlas reads from relay: the topics seen, and the newest events, for
// each node's last event and url-to-bucket's observed downloads; and the
// runs that started, for what is running now.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type event struct {
	ID    uint64         `json:"id"`
	Time  time.Time      `json:"time"`
	Topic string         `json:"topic"`
	Type  string         `json:"type"`
	RunID string         `json:"run_id,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// recent is what relay told us, newest event first. Err is set if relay
// couldn't be reached; the graph then has no live state.
type recent struct {
	Topics []string
	Events []event // the newest 1000, newest first
	Downs  []event // url-to-bucket.download ok, the newest 1000, newest first
	Err    string
}

type relayClient struct {
	url, token string
	client     *http.Client
}

func (r *relayClient) fetch(ctx context.Context) *recent {
	if r.url == "" {
		return &recent{Err: "RELAY_URL is not set"}
	}
	var out recent
	var topics []struct {
		Topic string `json:"topic"`
	}
	if err := r.get(ctx, "/v1/topics", &topics); err != nil {
		return &recent{Err: err.Error()}
	}
	for _, t := range topics {
		out.Topics = append(out.Topics, t.Topic)
	}
	var evs struct {
		Events []event `json:"events"`
	}
	if err := r.get(ctx, "/v1/events?last=1000", &evs); err != nil {
		return &recent{Err: err.Error()}
	}
	out.Events = newestFirst(evs.Events)
	evs.Events = nil
	if err := r.get(ctx, "/v1/events?last=1000&type=ok&topic="+url.QueryEscape("url-to-bucket.download"), &evs); err != nil {
		return &recent{Err: err.Error()}
	}
	out.Downs = newestFirst(evs.Events)
	return &out
}

// runEvents is what relay told us about runs: the newest starts and
// requests, the newest results, and every event of each run whose result
// isn't among those.
type runEvents struct {
	Starts []event            // started and requested, newest first
	Ends   []event            // ok and failed, newest first
	ByRun  map[string][]event // run ID -> its events, newest first
}

func (r *relayClient) fetchRuns(ctx context.Context, now time.Time) (*runEvents, error) {
	if r.url == "" {
		return nil, fmt.Errorf("RELAY_URL is not set")
	}
	var evs struct {
		Events []event `json:"events"`
	}
	if err := r.get(ctx, "/v1/events?last=500&type=started,requested", &evs); err != nil {
		return nil, err
	}
	out := &runEvents{Starts: newestFirst(evs.Events), ByRun: map[string][]event{}}
	evs.Events = nil
	if err := r.get(ctx, "/v1/events?last=1000&type=ok,failed", &evs); err != nil {
		return nil, err
	}
	out.Ends = newestFirst(evs.Events)
	for _, id := range unanswered(out.Starts, out.Ends, now) {
		evs.Events = nil
		if err := r.get(ctx, "/v1/events?last=1000&run_id="+url.QueryEscape(id), &evs); err != nil {
			return nil, err
		}
		out.ByRun[id] = newestFirst(evs.Events)
	}
	return out, nil
}

func (r *relayClient) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.url, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func newestFirst(evs []event) []event {
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs
}

// topicMatch is relay's filter: "a.b" one topic, "a.*" every topic starting
// with "a.", "*" everything, and comma-separated lists of those.
func topicMatch(filter, topic string) bool {
	for _, f := range strings.Split(filter, ",") {
		f = strings.TrimSpace(f)
		switch {
		case f == "*" || f == topic:
			return true
		case strings.HasSuffix(f, ".*") && strings.HasPrefix(topic, strings.TrimSuffix(f, "*")):
			return true
		}
	}
	return false
}

// typeMatch: empty matches any type; otherwise one of a comma list.
func typeMatch(filter, typ string) bool {
	if strings.TrimSpace(filter) == "" {
		return true
	}
	for _, f := range strings.Split(filter, ",") {
		if strings.TrimSpace(f) == typ {
			return true
		}
	}
	return false
}

// app is the service a topic filter is about: its first segment.
func app(filter string) string {
	a, _, _ := strings.Cut(strings.TrimSpace(filter), ".")
	return a
}
