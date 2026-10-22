package main

// The semantic layer, through one mcp-hub profile: the only data analyst
// can reach. Its tools are offered to the model as they are, minus BSL's
// chart options: the page draws its own charts from the rows, and a chart
// spec would only fill the model's context.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxToolText = 8 << 10 // longest tool result the model sees

// chartParams are BSL's chart arguments, hidden from the model. get_chart is
// always sent as false.
var chartParams = []string{"get_chart", "chart_backend", "chart_format", "chart_spec"}

type semantic struct {
	url    string // the hub's MCP endpoint for the profile
	token  string
	prefix string // tool names the profile may offer, e.g. "semantic__"
}

type tokenTransport struct{ token string }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (s *semantic) connect(ctx context.Context) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "analyst", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: s.url, MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: tokenTransport{s.token}}}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp-hub (%s): %v", s.url, err)
	}
	return sess, nil
}

// tools lists the semantic layer's tools, without the chart options.
func (s *semantic) tools(ctx context.Context, sess *mcp.ClientSession) ([]toolDef, error) {
	var out []toolDef
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp-hub: listing tools: %v", err)
		}
		if !strings.HasPrefix(t.Name, s.prefix) {
			continue
		}
		var schema map[string]any
		b, _ := json.Marshal(t.InputSchema)
		json.Unmarshal(b, &schema)
		if props, ok := schema["properties"].(map[string]any); ok {
			for _, p := range chartParams {
				delete(props, p)
			}
		}
		out = append(out, toolDef{Name: t.Name, Description: t.Description, Schema: schema})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mcp-hub offers no %s* tools at %s", s.prefix, s.url)
	}
	return out, nil
}

// call runs one tool and returns its full result text, and whether it is an
// error.
func (s *semantic) call(ctx context.Context, sess *mcp.ClientSession, name string, rawArgs json.RawMessage) (string, bool) {
	if !strings.HasPrefix(name, s.prefix) {
		return fmt.Sprintf("there is no tool named %q", name), true
	}
	args := map[string]any{}
	if len(rawArgs) > 0 && string(rawArgs) != "null" {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "the arguments are not a valid JSON object", true
		}
	}
	for _, p := range chartParams {
		delete(args, p)
	}
	if strings.HasSuffix(name, "__query_model") || strings.HasSuffix(name, "__compare_periods") {
		args["get_chart"] = false
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error(), true
	}
	text := resultText(res)
	// BSL reports a failed query as {"error": ...} in a normal result.
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal([]byte(text), &e) == nil && e.Error != nil {
		return text, true
	}
	return text, res.IsError
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
	return fmt.Sprintf("%s\n[cut: %d more bytes. The user sees every row; ask for fewer rows (limit) if you need them all.]", cut, len(s)-len(cut))
}
