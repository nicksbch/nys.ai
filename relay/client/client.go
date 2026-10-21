// Package client publishes events to relay.
//
// It never gets in the application's way: a nil *Client, from FromEnv when
// neither RELAY_URL nor /etc/relay-client.env is set, turns every call into a
// no-op, and a relay that is down only costs a short timeout. Errors are
// returned for logging, never to fail the job itself.
//
//	bus := client.FromEnv() // RELAY_URL, RELAY_TOKEN or /etc/relay-client.env
//
//	bus.Publish(ctx, client.Event{Topic: "my-app.ingest", Type: "started", RunID: id})
//	bus.Go(client.Event{...}) // in a server: publishes in the background
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Event is what an application publishes. ID and Time are set by relay and
// filled in on the event Publish returns.
type Event struct {
	ID    uint64    `json:"id,omitempty"`
	Time  time.Time `json:"time"`
	Topic string    `json:"topic"`            // e.g. "url-to-bucket.download"
	Type  string    `json:"type"`             // e.g. "started", "ok", "failed"
	RunID string    `json:"run_id,omitempty"` // shared by the events of one run
	Cause uint64    `json:"cause,omitempty"`  // ID of the event that led to this one
	Key   string    `json:"key,omitempty"`    // idempotency key: a repeat within 24 h is not stored again
	Data  any       `json:"data,omitempty"`   // any JSON object
}

type Client struct {
	URL   string // base URL of relay, e.g. http://127.0.0.1:8091
	Token string // bearer token
	HTTP  *http.Client
}

// EnvFile is where relay's install.sh writes RELAY_URL and RELAY_TOKEN for
// applications on the same server.
const EnvFile = "/etc/relay-client.env"

// FromEnv returns a client configured from RELAY_URL and RELAY_TOKEN. If
// RELAY_URL is not in the environment (cron, "ssh host cmd", shells that don't
// read ~/.profile), both are read from EnvFile instead. It returns nil (a
// no-op client) if neither has a URL.
func FromEnv() *Client {
	u, token := os.Getenv("RELAY_URL"), os.Getenv("RELAY_TOKEN")
	if u == "" {
		vars := readEnvFile(EnvFile)
		u, token = vars["RELAY_URL"], vars["RELAY_TOKEN"]
	}
	if u == "" {
		return nil
	}
	return &Client{
		URL:   strings.TrimRight(u, "/"),
		Token: token,
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

// Publish posts one event, waits for relay to store it and returns the stored
// event (or the original one, if e.Key was already used).
func (c *Client) Publish(ctx context.Context, e Event) (Event, error) {
	if c == nil {
		return Event{}, nil
	}
	body, err := json.Marshal(struct {
		Topic string `json:"topic"`
		Type  string `json:"type"`
		RunID string `json:"run_id,omitempty"`
		Cause uint64 `json:"cause,omitempty"`
		Key   string `json:"key,omitempty"`
		Data  any    `json:"data,omitempty"`
	}{e.Topic, e.Type, e.RunID, e.Cause, e.Key, e.Data})
	if err != nil {
		return Event{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/events", bytes.NewReader(body))
	if err != nil {
		return Event{}, err
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
		return Event{}, fmt.Errorf("relay: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var msg struct{ Error string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if json.Unmarshal(b, &msg) != nil || msg.Error == "" {
			msg.Error = strings.TrimSpace(string(b))
		}
		return Event{}, fmt.Errorf("relay: %s: %s", resp.Status, msg.Error)
	}
	var stored Event
	if err := json.NewDecoder(resp.Body).Decode(&stored); err != nil {
		return Event{}, fmt.Errorf("relay: reading response: %w", err)
	}
	return stored, nil
}

// Go publishes an event in the background and ignores the outcome. Use it in
// long-running servers; a CLI should use Publish so the event isn't lost at exit.
func (c *Client) Go(e Event) {
	if c == nil {
		return
	}
	go c.Publish(context.Background(), e)
}
