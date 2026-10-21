package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// Alert is a condition that is currently firing.
type Alert struct {
	Key     string    `json:"key"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// alerter tracks firing conditions and posts each change to a webhook. A
// condition is announced once when it starts and once when it clears, not on
// every check.
type alerter struct {
	webhook  string
	host     string
	client   *http.Client
	mu       sync.Mutex
	active   map[string]Alert
	lastSent map[string]time.Time // for one-off notices
}

func newAlerter(webhook string) *alerter {
	host, _ := os.Hostname()
	return &alerter{
		webhook:  webhook,
		host:     host,
		client:   &http.Client{Timeout: 10 * time.Second},
		active:   map[string]Alert{},
		lastSent: map[string]time.Time{},
	}
}

// set records whether the condition key is firing, announcing changes.
func (a *alerter) set(key string, firing bool, msg string) {
	a.mu.Lock()
	_, was := a.active[key]
	switch {
	case firing && !was:
		a.active[key] = Alert{Key: key, Message: msg, Since: time.Now()}
	case firing:
		al := a.active[key]
		al.Message = msg // keep the numbers current on the dashboard
		a.active[key] = al
	case was:
		delete(a.active, key)
	}
	a.mu.Unlock()

	if firing && !was {
		a.send(key, "firing", msg)
	} else if !firing && was {
		a.send(key, "resolved", "resolved: "+msg)
	}
}

// notice announces a one-off event (such as a failed ingestion), at most once
// per cooldown for the same key.
func (a *alerter) notice(key, msg string, cooldown time.Duration) {
	a.mu.Lock()
	if time.Since(a.lastSent[key]) < cooldown {
		a.mu.Unlock()
		return
	}
	a.lastSent[key] = time.Now()
	a.mu.Unlock()
	a.send(key, "notice", msg)
}

func (a *alerter) list() []Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Alert, 0, len(a.active))
	for _, al := range a.active {
		out = append(out, al)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// send logs the alert and posts it to the webhook in the background. The body
// works with Slack, Mattermost and Discord incoming webhooks ("text" and
// "content"), and carries structured fields for anything else.
func (a *alerter) send(key, state, msg string) {
	text := "[vigil " + a.host + "] " + msg
	log.Printf("alert %s %s: %s", state, key, msg)
	if a.webhook == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{
		"text": text, "content": text,
		"host": a.host, "alert": key, "state": state, "message": msg,
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.webhook, bytes.NewReader(body))
		if err != nil {
			log.Printf("webhook: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.client.Do(req)
		if err != nil {
			log.Printf("webhook: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("webhook: %s", resp.Status)
		}
	}()
}
