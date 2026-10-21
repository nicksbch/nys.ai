package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	relay "relay/client"
)

//go:embed usage.txt
var usage string

const maxAuditText = 2 << 10 // longest args or error text in an audit event

// hub joins the tools of every upstream into one MCP server per profile,
// plus one ("" here, /mcp on the wire) that shows everything.
type hub struct {
	cfg       *Config
	token     string
	bus       *relay.Client
	upstreams []*upstream
	servers   map[string]*mcp.Server // profile name -> server; "" -> every tool

	mu    sync.Mutex
	tools map[string][]route // upstream name -> its tools as the hub shows them
}

// route is one upstream tool under its hub name.
type route struct {
	tool     *mcp.Tool // a copy, renamed to <server>__<tool>
	upstream *upstream
	name     string // the tool's name on the upstream
}

func newHub(cfg *Config, token string, bus *relay.Client) *hub {
	h := &hub{cfg: cfg, token: token, bus: bus, servers: map[string]*mcp.Server{}, tools: map[string][]route{}}
	newServer := func() *mcp.Server {
		// Advertise tools even while none are known yet, so agents ask again
		// when the list changes.
		return mcp.NewServer(&mcp.Implementation{Name: "mcp-hub", Version: "1"}, &mcp.ServerOptions{
			Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}})
	}
	h.servers[""] = newServer()
	for name := range cfg.Profiles {
		h.servers[name] = newServer()
	}
	for _, s := range cfg.Servers {
		h.upstreams = append(h.upstreams, newUpstream(s, h.setTools))
	}
	return h
}

func (h *hub) run(ctx context.Context) {
	for _, u := range h.upstreams {
		go u.run(ctx)
	}
}

// setTools replaces an upstream's tools on every server that shows them.
// nil means the upstream is down: its tools disappear.
func (h *hub) setTools(u *upstream, tools []*mcp.Tool) {
	var routes []route
	for _, t := range tools {
		name := u.conf.Name + "__" + t.Name
		if !toolNameRE.MatchString(name) {
			log.Printf("%s: leaving out tool %q: %q is not a valid tool name (A-Z a-z 0-9 _ -, at most 64)", u.conf.Name, t.Name, name)
			continue
		}
		if !objectSchema(t.InputSchema) {
			log.Printf("%s: leaving out tool %q: its input schema is not an object", u.conf.Name, t.Name)
			continue
		}
		c := *t
		c.Name = name
		routes = append(routes, route{tool: &c, upstream: u, name: t.Name})
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.tools[u.conf.Name]
	if sameTools(old, routes) {
		return // the timer found nothing new; don't tell agents the list changed
	}
	var oldNames []string
	for _, r := range old {
		oldNames = append(oldNames, r.tool.Name)
	}
	for profile, s := range h.servers {
		s.RemoveTools(oldNames...)
		for _, r := range routes {
			if profile == "" || h.cfg.Profiles[profile].shows(u.conf.Name, r.tool.Name) {
				addTool(s, r.tool, h.forward(profile, r))
			}
		}
	}
	h.tools[u.conf.Name] = routes
}

// addTool adds a tool, logging instead of crashing the hub if the SDK
// rejects what an upstream sent.
func addTool(s *mcp.Server, t *mcp.Tool, h mcp.ToolHandler) {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("leaving out tool %q: %v", t.Name, err)
		}
	}()
	s.AddTool(t, h)
}

func objectSchema(s any) bool {
	var m map[string]any
	b, err := json.Marshal(s)
	return err == nil && json.Unmarshal(b, &m) == nil && m["type"] == "object"
}

func sameTools(a, b []route) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, _ := json.Marshal(a[i].tool)
		y, _ := json.Marshal(b[i].tool)
		if string(x) != string(y) {
			return false
		}
	}
	return true
}

// forward returns the handler for one tool on one profile's server. The
// upstream's result is returned unchanged. When the hub can't get one, the
// agent gets a tool error saying why. A call is never retried, since some
// tools change things.
func (h *hub) forward(profile string, r route) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		args := req.Params.Arguments
		res, err := r.upstream.call(ctx, r.name, args)
		if err != nil {
			res = &mcp.CallToolResult{}
			res.SetError(err)
		}
		h.audit(profile, r, time.Since(start), args, res, callerRunID(req))
		return res, nil
	}
}

// audit publishes the call to relay as mcp-hub.call, in the background. If
// relay is down, the call still works and the event is lost.
// callerRunID is the X-Run-Id header a caller such as foreman sends, so the
// audit event can be linked to the run that made the call. Anything that
// isn't a valid relay run id is ignored.
func callerRunID(req *mcp.CallToolRequest) string {
	if req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	if id := req.Extra.Header.Get("X-Run-Id"); runIDRE.MatchString(id) {
		return id
	}
	return ""
}

var runIDRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (h *hub) audit(profile string, r route, d time.Duration, args json.RawMessage, res *mcp.CallToolResult, runID string) {
	data := map[string]any{
		"profile": profile, "server": r.upstream.conf.Name, "tool": r.name,
		"name":        r.tool.Name, // what vigil shows for the event
		"duration_ms": d.Milliseconds(),
		"args":        cut(string(args)),
	}
	typ := "ok"
	if res.IsError {
		typ = "failed"
		data["error"] = cut(resultText(res))
	}
	h.bus.Go(relay.Event{Topic: "mcp-hub.call", Type: typ, RunID: runID, Data: data})
}

func resultText(res *mcp.CallToolResult) string {
	var s []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			s = append(s, t.Text)
		}
	}
	return strings.Join(s, "\n")
}

// cut shortens s to maxAuditText bytes without splitting a character.
func cut(s string) string {
	if len(s) <= maxAuditText {
		return s
	}
	s = s[:maxAuditText]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// --- HTTP ---------------------------------------------------------------------

func (h *hub) routes() http.Handler {
	mux := http.NewServeMux()
	auth := func(next http.Handler) http.Handler { return bearerAuth(h.token, next) }
	mcpHandler := func(s *mcp.Server) http.Handler {
		return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
			&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute})
	}
	for profile, s := range h.servers {
		path := "/mcp"
		if profile != "" {
			path += "/" + profile
		}
		mux.Handle(path, auth(mcpHandler(s)))
	}
	mux.HandleFunc("GET /{$}", h.usage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.Handle("GET /v1/servers", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, h.statuses())
	})))
	mux.Handle("GET /v1/tools", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, h.toolList())
	})))
	return mux
}

func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"unauthorized"}`+"\n")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *hub) statuses() []status {
	out := make([]status, len(h.upstreams))
	for i, u := range h.upstreams {
		out[i] = u.status()
	}
	return out
}

type toolInfo struct {
	Name        string   `json:"name"`
	Server      string   `json:"server"`
	Description string   `json:"description"`
	Profiles    []string `json:"profiles"`
}

func (h *hub) toolList() []toolInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []toolInfo{}
	for server, routes := range h.tools {
		for _, r := range routes {
			ti := toolInfo{Name: r.tool.Name, Server: server, Description: r.tool.Description, Profiles: []string{}}
			for name, p := range h.cfg.Profiles {
				if p.shows(server, r.tool.Name) {
					ti.Profiles = append(ti.Profiles, name)
				}
			}
			sort.Strings(ti.Profiles)
			out = append(out, ti)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// usage is GET /: what the hub is, its profiles and the state of each
// upstream, as plain text for agents and people.
func (h *hub) usage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, usage)
	fmt.Fprintf(w, "\nServers:\n")
	for _, st := range h.statuses() {
		line := fmt.Sprintf("  %-20s %-4s %3d tools  %s", st.Name, st.State, st.Tools, st.URL)
		if st.LastError != "" {
			line += "  (" + st.LastError + ")"
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "\nProfiles:\n  %-20s every tool\n", "/mcp")
	names := make([]string, 0, len(h.cfg.Profiles))
	for name := range h.cfg.Profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		p := h.cfg.Profiles[name]
		line := fmt.Sprintf("  %-20s %s", "/mcp/"+name, strings.Join(p.Servers, ", "))
		if len(p.Allow) > 0 {
			line += "; allow " + strings.Join(p.Allow, ", ")
		}
		if len(p.Deny) > 0 {
			line += "; deny " + strings.Join(p.Deny, ", ")
		}
		fmt.Fprintln(w, line)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
