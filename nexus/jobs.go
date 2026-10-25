package main

// Cards: Start calls the proposed tool through the hub, once. A long tool
// returns the run_id and topic of the job it started, and the card follows
// that job on relay until it reports ok or failed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Variables so tests can shorten them.
var (
	callTimeoutTool = 70 * time.Second // a little over the hub's 60 s, so its own timeout message comes back
	relayWait       = "30s"            // relay long-poll
	relayRetry      = 5 * time.Second  // after relay couldn't be read
)

// startCard calls a proposed (or failed) card's tool in the background. The
// state moves to calling first, under the store's lock, so two clicks call
// once.
func (a *app) startCard(conv, id string) error {
	err := a.store.updateCard(conv, id, func(c *Card) error {
		if c.State != cardProposed && c.State != cardFailed {
			return fail(http.StatusConflict, "the card is %s: only a proposed or failed card can be started", c.State)
		}
		c.State, c.Attempts, c.Result, c.Error, c.Progress = cardCalling, c.Attempts+1, "", "", ""
		c.JobRunID, c.JobTopic, c.Told = "", "", false
		return nil
	}, false)
	if err != nil {
		return err
	}
	a.wg.Add(1)
	go func() { defer a.wg.Done(); a.callCard(conv, id) }()
	return nil
}

func (a *app) cancelCard(conv, id string) error {
	return a.store.updateCard(conv, id, func(c *Card) error {
		if c.State != cardProposed {
			return fail(http.StatusConflict, "the card is %s: only a proposed card can be cancelled", c.State)
		}
		c.State, c.Told = cardCancelled, false
		return nil
	}, false)
}

// end records how a card ended.
func (a *app) end(conv, id, state, result, errText string) {
	err := a.store.updateCard(conv, id, func(c *Card) error {
		c.State, c.Result, c.Error, c.Progress = state, result, errText, ""
		return nil
	}, false)
	if err != nil {
		a.logf("card %s/%s: %v", conv, id, err)
	}
}

func (a *app) callCard(conv, id string) {
	card := a.store.card(conv, id)
	if card == nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, callTimeoutTool)
	defer cancel()
	sess, err := a.hub.connect(ctx, conv)
	if err != nil {
		a.end(conv, id, cardFailed, "", err.Error())
		return
	}
	defer sess.Close()
	args, _ := parseArgs(card.Args)
	res, text, isErr := call(ctx, sess, card.Tool, args)
	switch {
	case a.ctx.Err() != nil:
		return // stopping: left calling, and marked interrupted at the next start
	case isErr:
		a.end(conv, id, cardFailed, "", text)
		return
	}
	runID, topic := jobOf(res)
	if runID == "" {
		a.end(conv, id, cardDone, text, "")
		return
	}
	err = a.store.updateCard(conv, id, func(c *Card) error {
		c.State, c.JobRunID, c.JobTopic, c.Progress = cardRunning, runID, topic, "requested"
		return nil
	}, false)
	if err != nil {
		a.logf("card %s/%s: %v", conv, id, err)
		return
	}
	a.follow(conv, id)
}

// resume settles what a restart left behind: a card left calling may or may
// not have run, so it is failed, never called again by itself; one left
// running is followed again.
func (a *app) resume() {
	for conv, cards := range a.store.allCards() {
		for _, c := range cards {
			switch c.State {
			case cardCalling:
				a.end(conv, c.ID, cardFailed, "", "interrupted: nexus stopped during the call, which may have happened")
			case cardRunning:
				a.wg.Add(1)
				go func() { defer a.wg.Done(); a.follow(conv, c.ID) }()
			}
		}
	}
}

// relayEvent is the part of a relay event a card uses.
type relayEvent struct {
	ID   uint64         `json:"id"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// follow reads a running card's job from relay, from its first event, until
// it reports ok or failed, the card is gone, or nexus stops. There is no
// deadline: a job that never reports leaves its card running.
func (a *app) follow(conv, id string) {
	card := a.store.card(conv, id)
	if card == nil || card.State != cardRunning {
		return
	}
	if a.relay == nil {
		a.setProgress(conv, id, "relay is not configured (RELAY_URL), so this job can't be followed")
		return
	}
	var after uint64
	for a.ctx.Err() == nil {
		if c := a.store.card(conv, id); c == nil || c.State != cardRunning || c.JobRunID != card.JobRunID {
			return
		}
		q := url.Values{"topic": {card.JobTopic}, "run_id": {card.JobRunID}, "type": {"started,ok,failed"},
			"after": {strconv.FormatUint(after, 10)}, "limit": {"100"}, "wait": {relayWait}}
		var resp struct {
			Events []relayEvent `json:"events"`
		}
		if err := a.relayGet(a.ctx, "/v1/events?"+q.Encode(), &resp); err != nil {
			if a.ctx.Err() == nil {
				a.logf("card %s/%s: relay: %v", conv, id, err)
				select {
				case <-a.ctx.Done():
				case <-time.After(relayRetry):
				}
			}
			continue
		}
		for _, ev := range resp.Events {
			after = ev.ID
			switch ev.Type {
			case "started":
				a.setProgress(conv, id, "started")
			case "ok":
				b, _ := json.Marshal(ev.Data)
				a.end(conv, id, cardDone, string(b), "")
				return
			case "failed":
				msg, _ := ev.Data["error"].(string)
				if msg == "" {
					msg = "the job failed"
				}
				a.end(conv, id, cardFailed, "", msg)
				return
			}
		}
	}
}

func (a *app) setProgress(conv, id, p string) {
	a.store.updateCard(conv, id, func(c *Card) error { c.Progress = p; return nil }, false)
}

func (a *app) relayGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.relay.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	if a.relay.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.relay.Token)
	}
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, out)
}
