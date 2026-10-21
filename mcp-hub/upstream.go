package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const connectTimeout = 10 * time.Second

// Variables so tests can shorten them.
var (
	retryDown = 30 * time.Second // how often a down upstream is tried again
	refreshUp = 5 * time.Minute  // how often an up upstream's tools are re-read
)

// upstream is one MCP server the hub forwards to. It keeps one client
// session open, re-reads the tool list on list_changed and on a timer, and
// reports every new list (nil when the server is down) to onTools.
type upstream struct {
	conf    ServerConf
	onTools func(u *upstream, tools []*mcp.Tool)
	client  *mcp.Client
	http    *http.Client
	poked   chan struct{}

	mu      sync.Mutex
	session *mcp.ClientSession
	up      bool
	tools   int
	lastErr string
	checked time.Time
}

func newUpstream(conf ServerConf, onTools func(*upstream, []*mcp.Tool)) *upstream {
	u := &upstream{conf: conf, onTools: onTools, poked: make(chan struct{}, 1),
		// No overall timeout: the session holds a long-lived stream open.
		http: &http.Client{Transport: bearerTransport{conf.token, http.DefaultTransport}}}
	u.client = mcp.NewClient(&mcp.Implementation{Name: "mcp-hub", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { u.poke() },
	})
	return u
}

// bearerTransport adds the upstream's token to every request.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

// poke asks run to re-read the tool list now.
func (u *upstream) poke() {
	select {
	case u.poked <- struct{}{}:
	default:
	}
}

func (u *upstream) run(ctx context.Context) {
	defer u.closeSession()
	for {
		wait := refreshUp
		if !u.sync(ctx) {
			wait = retryDown
		}
		select {
		case <-ctx.Done():
			return
		case <-u.poked:
		case <-time.After(wait):
		}
	}
}

// sync re-reads the tool list, opening a session first if there is none. A
// failure is tried once more on a fresh session, since the upstream may only
// have restarted and forgotten the old one.
func (u *upstream) sync(ctx context.Context) bool {
	tools, err := u.listTools(ctx, false)
	if err != nil && ctx.Err() == nil {
		tools, err = u.listTools(ctx, true)
	}
	u.mu.Lock()
	wasUp, oldErr := u.up, u.lastErr
	u.checked = time.Now()
	if err != nil {
		u.up, u.tools, u.lastErr = false, 0, err.Error()
		u.mu.Unlock()
		u.closeSession()
		if wasUp || oldErr != err.Error() {
			log.Printf("%s is down: %v", u.conf.Name, err)
		}
		u.onTools(u, nil)
		return false
	}
	u.up, u.tools, u.lastErr = true, len(tools), ""
	u.mu.Unlock()
	if !wasUp {
		log.Printf("%s is up with %d tools", u.conf.Name, len(tools))
	}
	u.onTools(u, tools)
	return true
}

func (u *upstream) listTools(ctx context.Context, fresh bool) ([]*mcp.Tool, error) {
	if fresh {
		u.closeSession()
	}
	cs, err := u.getSession(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	var tools []*mcp.Tool
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// getSession returns the open session or opens one. Only run (or check)
// calls it, so connecting outside the lock can't race with itself.
func (u *upstream) getSession(ctx context.Context) (*mcp.ClientSession, error) {
	u.mu.Lock()
	cs := u.session
	u.mu.Unlock()
	if cs != nil {
		return cs, nil
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cs, err := u.client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: u.conf.URL, HTTPClient: u.http, MaxRetries: -1}, nil)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.session = cs
	u.mu.Unlock()
	return cs, nil
}

func (u *upstream) closeSession() {
	u.mu.Lock()
	cs := u.session
	u.session = nil
	u.mu.Unlock()
	if cs != nil {
		cs.Close()
	}
}

// call forwards one tool call. The error, if any, is written for the agent:
// it says which server failed and how.
func (u *upstream) call(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	u.mu.Lock()
	cs, lastErr := u.session, u.lastErr
	u.mu.Unlock()
	if cs == nil {
		return nil, fmt.Errorf("%s is down: %s", u.conf.Name, lastErr)
	}
	ctx, cancel := context.WithTimeout(ctx, u.conf.timeout)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s did not answer within %s", u.conf.Name, u.conf.timeout)
		}
		u.poke() // the session may be gone; find out now rather than in 5 minutes
		return nil, fmt.Errorf("%s: %v", u.conf.Name, err)
	}
	return res, nil
}

// status is one row of /v1/servers and of check's output.
type status struct {
	Name      string    `json:"name"`
	URL       string    `json:"url,omitempty"`
	State     string    `json:"state"`
	Tools     int       `json:"tools"`
	LastError string    `json:"last_error,omitempty"`
	Checked   time.Time `json:"checked"`
}

func (u *upstream) status() status {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := status{Name: u.conf.Name, URL: u.conf.URL, State: "down", Tools: u.tools, LastError: u.lastErr, Checked: u.checked}
	if u.up {
		st.State = "up"
	}
	return st
}
