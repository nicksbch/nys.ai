// Package client connects to mcp-hub and helps hand its tools to a model,
// for the apps that run one (analyst, nexus, foreman).
//
//	sess, err := client.Connect(ctx, "nexus", "http://127.0.0.1:8099/mcp/nexus", token, runID)
//	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
//	text := client.CutForModel(client.ResultText(res), 8<<10, "")
package client

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

// transport sends the hub's token, and the run id when there is one, so
// mcp-hub's audit links each call to its run.
type transport struct{ token, runID string }

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	if t.runID != "" {
		r.Header.Set("X-Run-Id", t.runID)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// Connect opens a session on url, one of the hub's MCP endpoints (/mcp or
// /mcp/<profile>), as the client name. runID may be "".
func Connect(ctx context.Context, name, url, token, runID string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: transport{token, runID}}}, nil)
}

// ReadOnly reports whether a tool is marked read-only.
func ReadOnly(t *mcp.Tool) bool { return t.Annotations != nil && t.Annotations.ReadOnlyHint }

// Schema is a tool's input schema as a plain map, as a model API takes it.
func Schema(t *mcp.Tool) map[string]any {
	var schema map[string]any
	b, _ := json.Marshal(t.InputSchema)
	json.Unmarshal(b, &schema)
	return schema
}

// ParseArgs reads a tool call's arguments as an object.
func ParseArgs(raw json.RawMessage) (map[string]any, error) {
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("the arguments are not a valid JSON object")
		}
	}
	return args, nil
}

// ValidateArgs checks arguments against a tool's input schema.
func ValidateArgs(schema any, args map[string]any) error {
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

// ResultText is the text of an MCP result, or its structured content as
// JSON when there is no text.
func ResultText(res *mcp.CallToolResult) string {
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

// CutForModel shortens a result longer than max bytes and says so, so the
// model knows it didn't get everything. hint, if any, is added to that
// note, e.g. how to ask for less.
func CutForModel(s string, max int, hint string) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && s[len(cut)]&0xC0 == 0x80 { // not in the middle of a UTF-8 character
		cut = cut[:len(cut)-1]
	}
	if hint != "" {
		hint = ". " + hint
	}
	return fmt.Sprintf("%s\n[cut: %d more bytes%s]", cut, len(s)-len(cut), hint)
}

// JobOf returns the job a long tool started: its result's structured
// content holds run_id and topic. Both are "" for any other result.
func JobOf(res *mcp.CallToolResult) (runID, topic string) {
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
