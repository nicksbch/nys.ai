// A small web UI to query the DuckLake set up by ducklake-setup.sh: a SQL
// editor, the lake's tables and columns, and results as a table or a CSV
// download. The lake is attached read-only as "lake" and is the default
// database, so "SELECT * FROM raw.orders" works.
//
// Queries run in the official duckdb CLI (1.5 or newer), one fresh process
// per request. Each request is one statement that returns rows (SELECT, FROM,
// DESCRIBE, SHOW, SUMMARIZE, time travel with AT (VERSION => n), ...).
//
// Credentials come from the environment:
//
//	PGHOST, PGPORT, PGUSER, PGPASSWORD   the catalog, as libpq reads them
//	AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
//	AWS_REGION=garage                          (default: garage)
//	AWS_ENDPOINT_URL=http://192.168.1.169:3900 (default)
//
// Any SQL typed in the page runs with these credentials, so give it read-only
// ones (install.sh does).
//
// Usage:
//
//	go build -o lake-ui . && ./lake-ui    # then open http://127.0.0.1:8092
//	./lake-ui -listen :8092 -auth admin:secret
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

const (
	lakeAlias  = "lake"
	maxRunning = 4 // queries running at once; more wait for a free slot
)

type server struct {
	duck    *duck
	maxRows int
	timeout time.Duration
	slots   chan struct{}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8092", "address to serve the UI on")
	auth := flag.String("auth", os.Getenv("LAKE_UI_AUTH"), "require HTTP basic auth, as user:pass")
	duckdbBin := flag.String("duckdb", "duckdb", "DuckDB CLI (1.5 or newer) to run queries with")
	catalog := flag.String("catalog", envOr("LAKE_UI_CATALOG", "ducklake:postgres:dbname=ducklake"), "DuckLake catalog to attach")
	endpoint := flag.String("endpoint-url", envOr("AWS_ENDPOINT_URL", "http://192.168.1.169:3900"), "S3 endpoint")
	stateDir := flag.String("state-dir", "", "directory for DuckDB's extensions and temp files (default: DuckDB's own)")
	maxRows := flag.Int("max-rows", 10000, "rows shown per query (CSV downloads have no limit)")
	timeout := flag.Duration("timeout", 5*time.Minute, "longest a query may run")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := newDuck(ctx, *duckdbBin, *catalog, *endpoint, *stateDir)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{duck: d, maxRows: *maxRows, timeout: *timeout, slots: make(chan struct{}, maxRunning)}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           guard(*auth, s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("lake-ui on http://%s -> %s (S3 %s)", *listen, *catalog, *endpoint)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// guard adds optional basic auth, and requires a custom header on every
// POST so other web pages can't forge them (a cross-origin request with a
// custom header needs a CORS preflight, which we never grant).
func guard(auth string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != "" {
			user, pass, _ := r.BasicAuth()
			if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(auth)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="lake-ui"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Lake-UI") == "" {
			http.Error(w, "missing X-Lake-UI header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.Handle("GET /api/tables", handler(s.tables))
	mux.Handle("POST /api/query", handler(s.query))
	mux.Handle("POST /api/csv", handler(s.csv))
	return mux
}

// --- helpers ----------------------------------------------------------------

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return &httpError{code, fmt.Sprintf(format, args...)}
}

// handler turns a returned error into a JSON {"error": ...} response.
func handler(fn func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := fn(w, r)
		if err == nil {
			return
		}
		code := http.StatusBadGateway
		var he *httpError
		var se *sqlError
		if errors.As(err, &he) {
			code = he.code
		} else if errors.As(err, &se) {
			code = http.StatusBadRequest
		}
		// Mistakes in the SQL typed aren't worth a log line.
		if r.Context().Err() == nil && se == nil {
			log.Printf("%s %s: %v", r.Method, r.URL, err)
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(v)
}

// begin waits for a free query slot. done frees it.
func (s *server) begin(r *http.Request) (ctx context.Context, done func(), err error) {
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	select {
	case s.slots <- struct{}{}:
		return ctx, func() { <-s.slots; cancel() }, nil
	case <-ctx.Done():
		cancel()
		return nil, nil, s.stopped(ctx.Err())
	}
}

// stopped explains a query that hit the timeout.
func (s *server) stopped(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fail(http.StatusGatewayTimeout, "the query ran longer than %s and was stopped", s.timeout)
	}
	return err
}

// readSQL reads {"sql": "..."} and wraps it for running (see wrap).
func readSQL(r *http.Request) (string, error) {
	var req struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return "", fail(http.StatusBadRequest, "bad request body: %v", err)
	}
	return wrap(req.SQL)
}

// --- handlers ---------------------------------------------------------------

type column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type table struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	Columns []column `json:"columns"`
}

// tables lists the lake's tables with their columns.
func (s *server) tables(w http.ResponseWriter, r *http.Request) error {
	ctx, done, err := s.begin(r)
	if err != nil {
		return err
	}
	defer done()
	var out bytes.Buffer
	err = s.duck.run(ctx, ".output\n.mode json\n"+
		"SELECT schema_name, table_name, column_name, data_type FROM duckdb_columns() "+
		"WHERE database_name = "+ql(lakeAlias)+" ORDER BY schema_name, table_name, column_index;\n", &out)
	if err != nil {
		return s.stopped(err)
	}
	// No tables print nothing at all.
	tables := []*table{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var rows []struct {
			Schema string `json:"schema_name"`
			Table  string `json:"table_name"`
			Column string `json:"column_name"`
			Type   string `json:"data_type"`
		}
		if err := dec.Decode(&rows); err != nil {
			return fmt.Errorf("reading duckdb output: %w", err)
		}
		for _, c := range rows {
			if n := len(tables); n == 0 || tables[n-1].Schema != c.Schema || tables[n-1].Name != c.Table {
				tables = append(tables, &table{Schema: c.Schema, Name: c.Table})
			}
			t := tables[len(tables)-1]
			t.Columns = append(t.Columns, column{c.Column, c.Type})
		}
	}
	return writeJSON(w, http.StatusOK, tables)
}

type result struct {
	Columns   []column            `json:"columns"`
	Rows      [][]json.RawMessage `json:"rows"`
	Truncated bool                `json:"truncated"` // more than maxRows rows; only the first are in Rows
	Ms        int64               `json:"ms"`
}

// query runs one statement and returns up to maxRows of its rows. DESCRIBE
// gives the columns in order (and for an empty result, which the CLI prints
// nothing for), then the rows follow as JSON objects keyed by those names.
func (s *server) query(w http.ResponseWriter, r *http.Request) error {
	from, err := readSQL(r)
	if err != nil {
		return err
	}
	ctx, done, err := s.begin(r)
	if err != nil {
		return err
	}
	defer done()
	start := time.Now()
	var out bytes.Buffer
	err = s.duck.run(ctx, ".output\n.mode json\n"+
		"DESCRIBE SELECT * FROM "+from+";\n"+
		"SELECT * FROM "+from+" LIMIT "+strconv.Itoa(s.maxRows+1)+";\n", &out)
	if err != nil {
		return s.stopped(err)
	}
	res, err := parseResult(out.Bytes(), s.maxRows)
	if err != nil {
		return err
	}
	res.Ms = time.Since(start).Milliseconds()
	return writeJSON(w, http.StatusOK, res)
}

func parseResult(out []byte, maxRows int) (*result, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var desc []struct {
		Name string `json:"column_name"`
		Type string `json:"column_type"`
	}
	if err := dec.Decode(&desc); err != nil {
		return nil, fmt.Errorf("reading duckdb output: %w", err)
	}
	var rows []map[string]json.RawMessage
	for dec.More() {
		var part []map[string]json.RawMessage
		if err := dec.Decode(&part); err != nil {
			return nil, fmt.Errorf("reading duckdb output: %w", err)
		}
		rows = append(rows, part...)
	}

	res := &result{Columns: make([]column, len(desc)), Rows: make([][]json.RawMessage, 0, min(len(rows), maxRows))}
	for i, c := range desc {
		res.Columns[i] = column{c.Name, c.Type}
	}
	if len(rows) > maxRows {
		rows, res.Truncated = rows[:maxRows], true
	}
	for _, m := range rows {
		cells := make([]json.RawMessage, len(desc))
		for i, c := range desc {
			cells[i] = cell(m[c.Name])
		}
		res.Rows = append(res.Rows, cells)
	}
	return res, nil
}

// cell passes a value through, except integers too big for a JavaScript
// number (BIGINT, HUGEINT), which become strings so no digits are lost.
func cell(v json.RawMessage) json.RawMessage {
	if len(v) == 0 {
		return json.RawMessage("null")
	}
	if len(v) > 15 && (v[0] == '-' || '0' <= v[0] && v[0] <= '9') && !bytes.ContainsAny(v, ".eE") {
		q, _ := json.Marshal(string(v))
		return q
	}
	return v
}

// csv streams every row of one statement as CSV. DuckDB writes it straight
// to the CLI's stdout, which is the response.
func (s *server) csv(w http.ResponseWriter, r *http.Request) error {
	from, err := readSQL(r)
	if err != nil {
		return err
	}
	ctx, done, err := s.begin(r)
	if err != nil {
		return err
	}
	defer done()
	out := &lazyWriter{w: w}
	err = s.duck.run(ctx, "COPY (SELECT * FROM "+from+") TO '/dev/stdout' (FORMAT csv, HEADER, USE_TMP_FILE false);\n", out)
	if err != nil && out.started {
		// Too late for an error response: cut the download off, so it
		// fails in the browser instead of passing for a complete file.
		log.Printf("%s %s: %v", r.Method, r.URL, err)
		panic(http.ErrAbortHandler)
	}
	return s.stopped(err)
}

// lazyWriter sets the CSV headers on the first write, so an error before any
// output can still be sent as JSON.
type lazyWriter struct {
	w       http.ResponseWriter
	started bool
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		l.w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		l.w.Header().Set("Content-Disposition", `attachment; filename="lake.csv"`)
	}
	return l.w.Write(p)
}
