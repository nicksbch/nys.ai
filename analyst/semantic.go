package main

// The semantic layer, through one mcp-hub profile: the only data analyst
// can reach. Its tools are offered to the model as they are, minus BSL's
// chart options: the page draws its own charts from the rows, and a chart
// spec would only fill the model's context.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"chatapi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	mcphub "mcp-hub/client"
)

const maxToolText = 8 << 10 // longest tool result the model sees

// cutHint is added to a result cut for the model.
const cutHint = "The user sees every row; ask for fewer rows (limit) if you need them all."

// chartParams are BSL's chart arguments, hidden from the model. get_chart is
// always sent as false.
var chartParams = []string{"get_chart", "chart_backend", "chart_format", "chart_spec"}

type semantic struct {
	url    string // the hub's MCP endpoint for the profile
	token  string
	prefix string // tool names the profile may offer, e.g. "semantic__"
}

func (s *semantic) connect(ctx context.Context) (*mcp.ClientSession, error) {
	sess, err := mcphub.Connect(ctx, "analyst", s.url, s.token, "")
	if err != nil {
		return nil, fmt.Errorf("mcp-hub (%s): %v", s.url, err)
	}
	return sess, nil
}

// tools lists the semantic layer's tools, without the chart options.
func (s *semantic) tools(ctx context.Context, sess *mcp.ClientSession) ([]chatapi.ToolDef, error) {
	var out []chatapi.ToolDef
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp-hub: listing tools: %v", err)
		}
		if !strings.HasPrefix(t.Name, s.prefix) {
			continue
		}
		schema := mcphub.Schema(t)
		if props, ok := schema["properties"].(map[string]any); ok {
			for _, p := range chartParams {
				delete(props, p)
			}
		}
		out = append(out, chatapi.ToolDef{Name: t.Name, Description: t.Description, Schema: schema})
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
	args, err := mcphub.ParseArgs(rawArgs)
	if err != nil {
		return err.Error(), true
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
	text := mcphub.ResultText(res)
	// BSL reports a failed query as {"error": ...} in a normal result.
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal([]byte(text), &e) == nil && e.Error != nil {
		return text, true
	}
	return text, res.IsError
}
