package main

// The MCP tool at /mcp, so an agent can ask for a download through mcp-hub
// (url-to-bucket__download). It publishes the same "requested" event as the
// page and returns the run's ID at once; the download reports on relay as
// any other. See SPEC-mcp.md.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerAuth requires "Authorization: Bearer <token>".
func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type downloadIn struct {
	URL         string `json:"url" jsonschema:"the http or https URL to download"`
	Bucket      string `json:"bucket" jsonschema:"the bucket to write to"`
	Key         string `json:"key,omitempty" jsonschema:"the object key; ending in / puts the file under that prefix with its own name; empty uses the file's name"`
	ContentType string `json:"content_type,omitempty" jsonschema:"the Content-Type to store; empty uses the server's"`
}

type downloadOut struct {
	RunID string `json:"run_id"`
	Topic string `json:"topic"`
}

func (u *ui) mcpHandler() http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "url-to-bucket", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "download",
		Description: "Download a URL into a bucket. It returns at once with the run's run_id; the download " +
			"then reports started, and ok or failed, on relay topic " + topic + " with that run_id. " +
			"Don't retry a URL that failed with a 4xx; it will fail the same way.",
		Meta: mcp.Meta{"wait": "2h"}}, u.toolDownload)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
}

func (u *ui) toolDownload(ctx context.Context, req *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, downloadOut, error) {
	data, _ := json.Marshal(in)
	runID, _, err := u.publishRequest(ctx, data)
	if err != nil {
		return nil, downloadOut{}, err
	}
	return nil, downloadOut{RunID: runID, Topic: topic}, nil
}
