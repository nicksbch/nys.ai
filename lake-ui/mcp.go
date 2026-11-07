package main

// The MCP tools at /mcp, so an agent can browse the lake's tables and run
// read-only SQL on them through mcp-hub (lake-ui__*). They do what the page
// does, with the same code, the same query slots and the same read-only
// credentials, and cut results down to a size a model can read. See
// SPEC-mcp.md.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultLimit = 100 // rows query returns by default
	maxLimit     = 500 // and at most
	maxCell      = 300 // characters of a cell; longer ones are cut
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

type listIn struct {
	Schema string `json:"schema,omitempty" jsonschema:"only this schema's tables, e.g. staging or uk; empty for every schema"`
}

type tableInfo struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Columns int    `json:"columns"` // how many; describe_table lists them
}

type listOut struct {
	Tables []tableInfo `json:"tables"`
}

type describeIn struct {
	Table string `json:"table" jsonschema:"the table as schema.table, e.g. staging.stg_onspd"`
}

type describeOut struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	Columns []column `json:"columns"`
	Rows    int64    `json:"rows"`
}

type queryIn struct {
	SQL   string `json:"sql" jsonschema:"one read-only statement that returns rows: SELECT, FROM, DESCRIBE, SHOW, SUMMARIZE, time travel with AT (VERSION => n)"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many rows to return; default 100, at most 500"`
}

type queryOut struct {
	Columns   []column `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"` // rows were dropped past limit, or a cell was cut
	Ms        int64    `json:"ms"`
}

func (s *server) mcpHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "lake-ui", Version: "1"}, nil)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(srv, &mcp.Tool{Name: "list_tables", Annotations: readOnly,
		Description: "The DuckLake's tables: schema, name and number of columns, for every schema or one. Raw tables " +
			"loaded by duckpipe are in a schema per area (e.g. uk.onspd); staging has the cleaned views (stg_<table>), " +
			"marts the aggregates; duckpipe.load_history and lakeshape.run_history say what was loaded and built."}, s.toolList)
	mcp.AddTool(srv, &mcp.Tool{Name: "describe_table", Annotations: readOnly,
		Description: "One table's columns (name and type) and its number of rows."}, s.toolDescribe)
	mcp.AddTool(srv, &mcp.Tool{Name: "query", Annotations: readOnly,
		Description: "Run one read-only SQL statement on the DuckLake (DuckDB SQL; the lake is the default database, " +
			"so FROM uk.onspd works) and get its columns and up to limit rows. Raw tables have tens of millions of rows " +
			"and keep every release: filter, aggregate or LIMIT rather than reading many rows. Cells longer than 300 " +
			"characters are cut, and truncated is true when rows were dropped or a cell cut. Also queryable: " +
			"ducklake_snapshots('lake'), and any table as it was with FROM t AT (VERSION => n). Writes are refused. " +
			"A query is stopped after 110 s."}, s.toolQuery)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true})
}

// mcpBegin waits for a query slot, with the MCP call timeout.
func (s *server) mcpBegin(ctx context.Context) (context.Context, func(), error) {
	return s.begin(ctx, s.mcpTimeout)
}

func (s *server) toolList(ctx context.Context, req *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
	ctx, done, err := s.mcpBegin(ctx)
	if err != nil {
		return nil, listOut{}, err
	}
	defer done()
	tables, err := s.listTables(ctx)
	if err != nil {
		return nil, listOut{}, stopped(err, s.mcpTimeout)
	}
	out := listOut{Tables: []tableInfo{}}
	for _, t := range tables {
		if in.Schema == "" || strings.EqualFold(t.Schema, in.Schema) {
			out.Tables = append(out.Tables, tableInfo{t.Schema, t.Name, len(t.Columns)})
		}
	}
	if len(out.Tables) == 0 && in.Schema != "" {
		return nil, listOut{}, fmt.Errorf("there is no schema %q: the schemas are %s", in.Schema, schemas(tables))
	}
	return nil, out, nil
}

func (s *server) toolDescribe(ctx context.Context, req *mcp.CallToolRequest, in describeIn) (*mcp.CallToolResult, describeOut, error) {
	schema, name, ok := strings.Cut(strings.TrimSpace(in.Table), ".")
	if !ok || schema == "" || name == "" {
		return nil, describeOut{}, fmt.Errorf("table is schema.table, e.g. staging.stg_onspd, not %q", in.Table)
	}
	ctx, done, err := s.mcpBegin(ctx)
	if err != nil {
		return nil, describeOut{}, err
	}
	defer done()
	tables, err := s.listTables(ctx)
	if err != nil {
		return nil, describeOut{}, stopped(err, s.mcpTimeout)
	}
	i := slices.IndexFunc(tables, func(t *table) bool {
		return strings.EqualFold(t.Schema, schema) && strings.EqualFold(t.Name, name)
	})
	if i < 0 {
		return nil, describeOut{}, fmt.Errorf("there is no table %q: the schemas are %s (list_tables lists their tables)", in.Table, schemas(tables))
	}
	t := tables[i]
	// The names come from the catalog, quoted as identifiers.
	var buf bytes.Buffer
	err = s.duck.run(ctx, ".output\n.mode json\nSELECT count(*) AS n FROM "+qi(lakeAlias)+"."+qi(t.Schema)+"."+qi(t.Name)+";\n", &buf)
	if err != nil {
		return nil, describeOut{}, stopped(err, s.mcpTimeout)
	}
	var count []struct {
		N int64 `json:"n"`
	}
	if err := json.Unmarshal(buf.Bytes(), &count); err != nil || len(count) != 1 {
		return nil, describeOut{}, fmt.Errorf("reading duckdb output: %q", buf.String())
	}
	return nil, describeOut{Schema: t.Schema, Name: t.Name, Columns: t.Columns, Rows: count[0].N}, nil
}

func (s *server) toolQuery(ctx context.Context, req *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, queryOut, error) {
	from, err := wrap(in.SQL)
	if err != nil {
		return nil, queryOut{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)
	ctx, done, err := s.mcpBegin(ctx)
	if err != nil {
		return nil, queryOut{}, err
	}
	defer done()
	res, err := s.runQuery(ctx, from, limit)
	if err != nil {
		return nil, queryOut{}, stopped(err, s.mcpTimeout)
	}
	out := queryOut{Columns: res.Columns, Rows: make([][]any, len(res.Rows)), Truncated: res.Truncated, Ms: res.Ms}
	for i, row := range res.Rows {
		out.Rows[i] = make([]any, len(row))
		for j, v := range row {
			var cut bool
			out.Rows[i][j], cut = cutCell(v)
			out.Truncated = out.Truncated || cut
		}
	}
	return nil, out, nil
}

// cutCell decodes one cell (numbers keep their digits) and cuts a string
// longer than maxCell characters. A list or struct whose JSON is longer
// becomes that JSON, cut, as a string.
func cutCell(v json.RawMessage) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return string(v), false
	}
	str, isString := x.(string)
	if !isString {
		if utf8.RuneCount(v) <= maxCell {
			return x, false
		}
		str = string(v)
	}
	if utf8.RuneCountInString(str) <= maxCell {
		return str, false
	}
	return string([]rune(str)[:maxCell]) + "…", true
}

// schemas lists the schemas of tables, for an error.
func schemas(tables []*table) string {
	var names []string
	for _, t := range tables {
		if !slices.Contains(names, t.Schema) {
			names = append(names, t.Schema)
		}
	}
	if len(names) == 0 {
		return "none (the lake has no tables)"
	}
	return strings.Join(names, ", ")
}

// qi quotes s as an SQL identifier.
func qi(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
