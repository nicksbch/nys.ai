package main

// The tools of one mcp-hub profile: everything nexus can do. Read-only
// tools run during a turn; the others are proposed as cards and run when
// you press Start.

import (
	"context"
	"fmt"

	"chatapi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	mcphub "mcp-hub/client"
)

const maxToolText = 8 << 10 // longest tool result the model sees

type hubClient struct {
	url   string // the hub's MCP endpoint for the profile
	token string
}

// connect opens a session that sends the conversation's id as the run id,
// so mcp-hub's audit links each call to it.
func (h *hubClient) connect(ctx context.Context, runID string) (*mcp.ClientSession, error) {
	sess, err := mcphub.Connect(ctx, "nexus", h.url, h.token, runID)
	if err != nil {
		return nil, fmt.Errorf("mcp-hub (%s): %v", h.url, err)
	}
	return sess, nil
}

// toolSet is the profile's tools, for the model and by name.
type toolSet struct {
	defs   []chatapi.ToolDef
	byName map[string]*mcp.Tool
}

func (h *hubClient) tools(ctx context.Context, sess *mcp.ClientSession) (*toolSet, error) {
	ts := &toolSet{byName: map[string]*mcp.Tool{}}
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp-hub: listing tools: %v", err)
		}
		desc := t.Description
		if !mcphub.ReadOnly(t) {
			desc += proposeNote
		}
		ts.defs = append(ts.defs, chatapi.ToolDef{Name: t.Name, Description: desc, Schema: mcphub.Schema(t)})
		ts.byName[t.Name] = t
	}
	if len(ts.defs) == 0 {
		return nil, fmt.Errorf("mcp-hub offers no tools at %s", h.url)
	}
	return ts, nil
}

// call runs one tool and returns its result, its text, and whether it is
// an error.
func call(ctx context.Context, sess *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string, bool) {
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err.Error(), true
	}
	return res, mcphub.ResultText(res), res.IsError
}
