package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type tokenTransport struct{ token string }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	srv := httptest.NewServer(testServer(t).handler("admin:pw", "mcp-token"))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: tokenTransport{token}}, DisableStandaloneSSE: true}, nil)
}

// call calls a tool and decodes its structured result into out. It returns
// the error text of a tool error, or "".
func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var msg []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg = append(msg, tc.Text)
			}
		}
		return strings.Join(msg, " ")
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v in %s", name, err, b)
	}
	return ""
}

func TestMCPTables(t *testing.T) {
	s, err := connect(t, "mcp-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var list listOut
	if msg := call(t, s, "list_tables", map[string]any{"schema": "raw"}, &list); msg != "" ||
		len(list.Tables) != 1 || list.Tables[0] != (tableInfo{"raw", "orders", 3}) {
		t.Errorf("list_tables raw: %q %+v", msg, list)
	}
	if msg := call(t, s, "list_tables", map[string]any{"schema": "nope"}, &list); !strings.Contains(msg, "the schemas are raw") {
		t.Errorf("list_tables nope: %q", msg)
	}

	var desc describeOut
	if msg := call(t, s, "describe_table", map[string]any{"table": "raw.orders"}, &desc); msg != "" ||
		desc.Rows != 3 || len(desc.Columns) != 3 || desc.Columns[1] != (column{"note", "VARCHAR"}) {
		t.Errorf("describe_table: %q %+v", msg, desc)
	}
	for _, table := range []string{"raw.nope", "orders"} {
		if msg := call(t, s, "describe_table", map[string]any{"table": table}, &desc); msg == "" {
			t.Errorf("describe_table %q: want an error", table)
		}
	}
}

func TestMCPQuery(t *testing.T) {
	s, err := connect(t, "mcp-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var q queryOut
	if msg := call(t, s, "query", map[string]any{"sql": "SELECT id, big FROM raw.orders ORDER BY id"}, &q); msg != "" ||
		len(q.Rows) != 3 || q.Truncated || q.Rows[0][1] != "12345678901234567890" || q.Rows[1][0] != float64(2) {
		t.Errorf("query: %q %+v", msg, q)
	}

	// limit, and its cap.
	for limit, want := range map[int]int{0: defaultLimit, 7: 7, 1000: maxLimit} {
		if msg := call(t, s, "query", map[string]any{"sql": "FROM range(600)", "limit": limit}, &q); msg != "" ||
			len(q.Rows) != want || !q.Truncated {
			t.Errorf("limit %d: %q %d rows, truncated %v", limit, msg, len(q.Rows), q.Truncated)
		}
	}

	// Long cells are cut, a string or a list.
	if msg := call(t, s, "query", map[string]any{"sql": "SELECT repeat('é', 400) AS s, [repeat('y', 400)] AS l, 'short' AS x"}, &q); msg != "" ||
		!q.Truncated || q.Rows[0][2] != "short" {
		t.Fatalf("long cells: %q %+v", msg, q)
	}
	for i := range 2 {
		if c, _ := q.Rows[0][i].(string); len([]rune(c)) != maxCell+1 || !strings.HasSuffix(c, "…") {
			t.Errorf("cell %d not cut: %v", i, q.Rows[0][i])
		}
	}

	// What the page refuses, the tool refuses.
	for _, sql := range []string{"CREATE TABLE raw.x (i INT)", "SELECT 1; SELECT 2", ""} {
		if msg := call(t, s, "query", map[string]any{"sql": sql}, &q); msg == "" || strings.Contains(msg, "LINE 1") {
			t.Errorf("%q: want a clean error, got %q", sql, msg)
		}
	}
}

func TestMCPAuth(t *testing.T) {
	if _, err := connect(t, "wrong"); err == nil {
		t.Error("connected with a wrong token")
	}
	// The page's password doesn't open /mcp, and the token doesn't open the page.
	srv := httptest.NewServer(testServer(t).handler("admin:pw", "mcp-token"))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{}`))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("basic auth on /mcp: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/api/tables", nil)
	req.Header.Set("Authorization", "Bearer mcp-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("token on /api/tables: %d", resp.StatusCode)
	}
}
