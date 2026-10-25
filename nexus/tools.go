package main

// The tools of one mcp-hub profile: everything nexus can do. Read-only
// tools run during a turn; the others are proposed as cards and run when
// you press Start.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxToolText = 8 << 10 // longest tool result the model sees

type hubClient struct {
	url   string // the hub's MCP endpoint for the profile
	token string
}

// hubTransport sends the hub's token, and the conversation's id as the run
// id, so mcp-hub's audit links each call to it.
type hubTransport struct{ token, runID string }

func (t hubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	if t.runID != "" {
		r.Header.Set("X-Run-Id", t.runID)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (h *hubClient) connect(ctx context.Context, runID string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "nexus", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.url, MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: hubTransport{h.token, runID}}}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp-hub (%s): %v", h.url, err)
	}
	return sess, nil
}

// toolSet is the profile's tools, for the model and by name.
type toolSet struct {
	defs   []toolDef
	byName map[string]*mcp.Tool
}

func (h *hubClient) tools(ctx context.Context, sess *mcp.ClientSession) (*toolSet, error) {
	ts := &toolSet{byName: map[string]*mcp.Tool{}}
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp-hub: listing tools: %v", err)
		}
		var schema map[string]any
		b, _ := json.Marshal(t.InputSchema)
		json.Unmarshal(b, &schema)
		desc := t.Description
		if !readOnly(t) {
			desc += proposeNote
		}
		ts.defs = append(ts.defs, toolDef{Name: t.Name, Description: desc, Schema: schema})
		ts.byName[t.Name] = t
	}
	if len(ts.defs) == 0 {
		return nil, fmt.Errorf("mcp-hub offers no tools at %s", h.url)
	}
	return ts, nil
}

func readOnly(t *mcp.Tool) bool { return t.Annotations != nil && t.Annotations.ReadOnlyHint }

// parseArgs reads a tool call's arguments as an object.
func parseArgs(raw json.RawMessage) (map[string]any, error) {
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("the arguments are not a valid JSON object")
		}
	}
	return args, nil
}

// validateArgs checks arguments against a tool's input schema.
func validateArgs(schema any, args map[string]any) error {
	if schema == nil {
		return nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("the tool's schema can't be read: %v", err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		return fmt.Errorf("the tool's schema can't be used: %v", err)
	}
	return rs.Validate(args)
}

// call runs one tool and returns its result, its text, and whether it is
// an error.
func call(ctx context.Context, sess *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string, bool) {
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err.Error(), true
	}
	return res, resultText(res), res.IsError
}

// jobOf returns the job a long tool started: its result's structured
// content holds run_id and topic. Both are "" for any other result.
func jobOf(res *mcp.CallToolResult) (runID, topic string) {
	if res == nil {
		return "", ""
	}
	m, _ := res.StructuredContent.(map[string]any)
	runID, _ = m["run_id"].(string)
	topic, _ = m["topic"].(string)
	if runID == "" || topic == "" {
		return "", ""
	}
	return runID, topic
}

// resultText is the text of an MCP result, or its structured content as
// JSON when there is no text.
func resultText(res *mcp.CallToolResult) string {
	var s []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			s = append(s, t.Text)
		}
	}
	if len(s) == 0 && res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		return string(b)
	}
	return strings.Join(s, "\n")
}

// cutForModel shortens a long result and says so, so the model knows it
// didn't get everything.
func cutForModel(s string) string {
	if len(s) <= maxToolText {
		return s
	}
	cut := s[:maxToolText]
	for len(cut) > 0 && s[len(cut)]&0xC0 == 0x80 { // not in the middle of a UTF-8 character
		cut = cut[:len(cut)-1]
	}
	return fmt.Sprintf("%s\n[cut: %d more bytes]", cut, len(s)-len(cut))
}
