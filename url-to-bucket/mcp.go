package main

// The MCP tools at /mcp, so an agent can ask for a download through mcp-hub
// (url-to-bucket__download). download publishes the same "requested" event
// as the page and returns the run's ID at once; the download reports on
// relay as any other. list_buckets says where downloads can go. See
// SPEC-mcp.md.

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

type bucketsOut struct {
	Buckets []string `json:"buckets"`
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
	mcp.AddTool(s, &mcp.Tool{Name: "list_buckets", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "The buckets downloads can go to: the bucket argument of download must be one of them."}, u.toolBuckets)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
}

func (u *ui) toolBuckets(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, bucketsOut, error) {
	names, err := u.bucketNames(ctx)
	if err != nil {
		return nil, bucketsOut{}, err
	}
	return nil, bucketsOut{Buckets: names}, nil
}

func (u *ui) toolDownload(ctx context.Context, req *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, downloadOut, error) {
	data, _ := json.Marshal(in)
	runID, _, err := u.publishRequest(ctx, data)
	if err != nil {
		return nil, downloadOut{}, err
	}
	return nil, downloadOut{RunID: runID, Topic: topic}, nil
}
