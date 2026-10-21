package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestWrap(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":              "query('SELECT 1')",
		"  SELECT 1 ; ;\n":      "query('SELECT 1')",
		"SELECT 'it''s', 'x'":   "query('SELECT ''it''''s'', ''x''')",
		"SELECT 1 -- a comment": "query('SELECT 1 -- a comment')",
	} {
		if got, err := wrap(in); err != nil || got != want {
			t.Errorf("wrap(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", " ; ", "SELECT '\x00'"} {
		if _, err := wrap(in); err == nil {
			t.Errorf("wrap(%q): want an error", in)
		}
	}
}

func TestCell(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "null",
		"null":                   "null",
		"42":                     "42",
		"-1.5":                   "-1.5",
		`"text"`:                 `"text"`,
		"12345678901234567890":   `"12345678901234567890"`,
		"-12345678901234567890":  `"-12345678901234567890"`,
		"1.2345678901234567e+30": "1.2345678901234567e+30",
	} {
		if got := string(cell(json.RawMessage(in))); got != want {
			t.Errorf("cell(%s) = %s; want %s", in, got, want)
		}
	}
}

func TestCleanError(t *testing.T) {
	got := cleanError("Parser Error: Expected a single SELECT statement\n\nLINE 1: SELECT * FROM query('CREATE TABLE t(x int)')\n   ^")
	if strings.Contains(got, "LINE") || !strings.Contains(got, "one read-only statement") {
		t.Errorf("cleanError: got %q", got)
	}
}

func TestParseResultEmpty(t *testing.T) {
	res, err := parseResult([]byte(`[{"column_name":"a","column_type":"INTEGER"}]`), 10)
	if err != nil || len(res.Columns) != 1 || res.Rows == nil || len(res.Rows) != 0 {
		t.Fatalf("got %+v, %v", res, err)
	}
}

// newTestServer serves lake-ui over a real duckdb CLI, with an in-memory
// database standing in for the lake.
func newTestServer(t *testing.T, auth string) *httptest.Server {
	t.Helper()
	bin, err := exec.LookPath("duckdb")
	if err != nil {
		t.Skip("duckdb CLI not on PATH")
	}
	d := &duck{bin: bin, setup: ".output /dev/null\n" + stmts(
		"ATTACH ':memory:' AS lake", "USE lake", "CREATE SCHEMA raw",
		"CREATE TABLE raw.orders (id INTEGER, note VARCHAR, big HUGEINT)",
		"INSERT INTO raw.orders VALUES (1, 'a', 12345678901234567890), (2, NULL, 0), (3, 'c,d', 1)",
	)}
	s := &server{duck: d, maxRows: 2, timeout: time.Minute, slots: make(chan struct{}, maxRunning)}
	ts := httptest.NewServer(guard(auth, s.routes()))
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, ts *httptest.Server, path, sql string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"sql": sql})
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(string(body)))
	req.Header.Set("X-Lake-UI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestQuery(t *testing.T) {
	ts := newTestServer(t, "")

	code, body := post(t, ts, "/api/query", "SELECT * FROM raw.orders ORDER BY id;")
	var res result
	if err := json.Unmarshal([]byte(body), &res); code != 200 || err != nil {
		t.Fatalf("query: %d %s", code, body)
	}
	if len(res.Columns) != 3 || res.Columns[2] != (column{"big", "HUGEINT"}) || !res.Truncated || len(res.Rows) != 2 {
		t.Fatalf("query: %s", body)
	}
	if got := string(res.Rows[0][2]) + " " + string(res.Rows[1][1]); got != `"12345678901234567890" null` {
		t.Errorf("cells: got %s", got)
	}

	// Columns come through even when there are no rows.
	code, body = post(t, ts, "/api/query", "FROM raw.orders WHERE false")
	if code != 200 || !strings.Contains(body, `"rows":[]`) || !strings.Contains(body, `"name":"note"`) {
		t.Errorf("empty result: %d %s", code, body)
	}

	// The SQL stays inside a string literal: a dot command in it is data.
	code, body = post(t, ts, "/api/query", "SELECT ';\n.shell echo PWNED\n' AS x")
	if code != 200 || !strings.Contains(body, `.shell echo PWNED`) {
		t.Errorf("dot command: %d %s", code, body)
	}

	for _, sql := range []string{"CREATE TABLE raw.x (i INT)", "DROP TABLE raw.orders", "SELECT 1; SELECT 2", "SELEC 1"} {
		if code, body := post(t, ts, "/api/query", sql); code != 400 || strings.Contains(body, "LINE 1") {
			t.Errorf("%q: want a clean 400, got %d %s", sql, code, body)
		}
	}
}

func TestCSV(t *testing.T) {
	ts := newTestServer(t, "")
	code, body := post(t, ts, "/api/csv", "SELECT id, note FROM raw.orders ORDER BY id")
	if want := "id,note\n1,a\n2,\n3,\"c,d\"\n"; code != 200 || body != want {
		t.Errorf("csv: %d %q; want %q", code, body, want)
	}
	if code, body := post(t, ts, "/api/csv", "SELECT nope"); code != 400 || !strings.Contains(body, "nope") {
		t.Errorf("csv error: %d %s", code, body)
	}
}

func TestTables(t *testing.T) {
	ts := newTestServer(t, "")
	resp, err := http.Get(ts.URL + "/api/tables")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tables []table
	json.NewDecoder(resp.Body).Decode(&tables)
	if len(tables) != 1 || tables[0].Schema != "raw" || tables[0].Name != "orders" || len(tables[0].Columns) != 3 || tables[0].Columns[0].Name != "id" {
		t.Errorf("tables: %+v", tables)
	}
}

func TestGuard(t *testing.T) {
	ts := newTestServer(t, "admin:secret")
	resp, _ := http.Get(ts.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no login: got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	req.SetBasicAuth("admin", "secret")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("POST without X-Lake-UI: got %d", resp.StatusCode)
	}
}
