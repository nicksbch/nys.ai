package main

// The MCP tools at /mcp, so an agent can see what the pipes are loading and
// what they loaded, and stop or resume loading, through mcp-hub
// (duckpipe__*). They do what the page does, with the same code. See
// SPEC-mcp.md.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultHistory = 20               // rows load_history returns by default
	historyTimeout = 50 * time.Second // inside mcp-hub's 60 s call timeout
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

type historyIn struct {
	Pipe   string `json:"pipe,omitempty" jsonschema:"only this pipe's files; empty for every pipe"`
	Status string `json:"status,omitempty" jsonschema:"only files with this status: loaded or failed; empty for both"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many history rows, newest first; default 20, at most 500"`
}

type doneOut struct {
	State string `json:"state"` // duckpipe's state after the call
}

func (e *engine) mcpHandler() http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "duckpipe", Version: "1"}, nil)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "list_pipes", Annotations: readOnly,
		Description: "What duckpipe is doing now: its state (loading, maintenance, idle, stopping, stopped), the batch " +
			"being loaded, and each pipe's source (s3://bucket/prefix/pattern), table, last poll and its error, and the " +
			"files it still has to load."}, e.toolPipes)
	mcp.AddTool(s, &mcp.Tool{Name: "load_history", Annotations: readOnly,
		Description: "Each pipe's totals (files and rows loaded, files failed, last load) and the newest load history " +
			"rows: when, pipe, file, table, status (loaded or failed), rows, size and the error of a failed file."}, e.toolHistory)
	mcp.AddTool(s, &mcp.Tool{Name: "stop_all",
		Description: "Stop all loading, as Stop all on duckpipe's page: the load under way is cancelled and rolled back, " +
			"and no pipe polls until resume. The files of the cancelled load are loaded again after resume."}, e.toolStop)
	mcp.AddTool(s, &mcp.Tool{Name: "resume",
		Description: "Resume loading after stop_all, as Resume on duckpipe's page: every pipe polls again, starting now."}, e.toolResume)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
}

func (e *engine) toolPipes(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, stateView, error) {
	return nil, e.stateView(), nil
}

func (e *engine) toolHistory(ctx context.Context, req *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, historyView, error) {
	if in.Pipe != "" && !slices.ContainsFunc(e.cfg.Pipes, func(p *pipe) bool { return p.Name == in.Pipe }) {
		return nil, historyView{}, fmt.Errorf("there is no pipe %q: list_pipes lists them", in.Pipe)
	}
	if in.Status != "" && in.Status != "loaded" && in.Status != "failed" {
		return nil, historyView{}, fmt.Errorf("status is loaded or failed, not %q", in.Status)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultHistory
	}
	limit = min(limit, historyLimit)
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	v, err := e.historyView(ctx, in.Pipe, in.Status, limit)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, historyView{}, fmt.Errorf("the lake didn't answer in %s", historyTimeout)
	}
	return nil, v, err
}

func (e *engine) toolStop(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, doneOut, error) {
	log.Printf("mcp: stop all")
	e.stop()
	return nil, doneOut{State: e.stateView().State}, nil
}

func (e *engine) toolResume(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, doneOut, error) {
	log.Printf("mcp: resume")
	e.resume()
	return nil, doneOut{State: e.stateView().State}, nil
}
