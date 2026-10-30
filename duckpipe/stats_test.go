package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCheckStatsOptions(t *testing.T) {
	for _, c := range []struct {
		format, pattern, options string
		ok                       bool
		delim                    rune
	}{
		{"", "*.csv", `header = true, all_varchar = true, normalize_names = true, quote = '"'`, true, ','},
		{"", "*.csv", "header = false, names = ['a, b', 'c'], delim = ';'", true, ';'},
		{"", "*.csv", `header = true, delim = '\t'`, true, '\t'},
		{"", "*.csv", "all_varchar = true", false, 0},          // no header
		{"", "*.csv", "header = true, skip = 2", false, 0},     // changes which lines are rows
		{"", "*.csv", "header = true, quote = ''''", false, 0}, // a quote encoding/csv doesn't know
		{"", "*.csv", "header = true, delim = '||'", false, 0}, // more than one character
		{"json", "", "format = 'array'", false, 0},             // not one row per line
		{"parquet", "", "binary_as_string = true", true, 0},    // counted from the footer
		{"", "*.json", "", true, 0},
	} {
		p := &pipe{Name: "p", Format: c.format, Pattern: c.pattern, Options: c.options}
		err := p.checkStatsOptions()
		if (err == nil) != c.ok {
			t.Errorf("%q: err = %v, want ok %v", c.options, err, c.ok)
		}
		if err == nil && c.delim != 0 && p.csv.delim != c.delim {
			t.Errorf("%q: delim %q, want %q", c.options, p.csv.delim, c.delim)
		}
	}
	// The deployed pipes all pass (TestDeployedConfig loads them with stats on).
}

func TestCountCSV(t *testing.T) {
	comma := csvOpts{delim: ',', header: true, headerSet: true}
	for _, c := range []struct {
		name, data      string
		records, fields int64
		fail            bool
	}{
		{"plain", "a,b\n1,2\n3,4\n", 2, 2, false},
		{"quoted comma and newline", "a,b\n\"x, y\",\"multi\nline\"\n3,4\n", 2, 2, false},
		{"crlf, BOM, no final newline", "\xef\xbb\xbf\"a\",b\r\n1,2\r\n3,4", 2, 2, false},
		{"blank line", "a,b\n1,2\n\n3,4\n", 2, 2, false},
		{"unquoted newline", "a,b\n1,2\n3\n,4\n", 0, 0, true},
		{"too many fields", "a,b\n1,2,3\n", 0, 0, true},
		{"bare quote", "a,b\n1,x\"y\n", 0, 0, true},
		{"header only", "a,b\n", 0, 2, false},
	} {
		records, fields, err := countCSV(strings.NewReader(c.data), comma)
		if (err != nil) != c.fail {
			t.Errorf("%s: err = %v", c.name, err)
			continue
		}
		if !c.fail && (records != c.records || fields != c.fields) {
			t.Errorf("%s: %d records, %d fields; want %d, %d", c.name, records, fields, c.records, c.fields)
		}
	}
	if n, _ := countLines(strings.NewReader("{\"a\":1}\n\n  \n{\"a\":2}")); n != 2 {
		t.Errorf("countLines = %d, want 2", n)
	}
}

// lakeEngine is an engine on a DuckLake in a temporary folder, reading its
// files from <root>/home/. It needs the duckdb CLI with the ducklake
// extension.
func lakeEngine(t *testing.T, options string) (*engine, *pipe, string) {
	t.Helper()
	bin, err := exec.LookPath("duckdb")
	if err != nil {
		t.Skip("no duckdb CLI")
	}
	dir := t.TempDir()
	cfg := &config{Pipes: []*pipe{{Name: "orders", Bucket: "home", Prefix: "in/", Table: "raw.orders", Options: options}}}
	cfg.Lake.Name, cfg.Lake.Catalog = "lake", "ducklake:"+filepath.Join(dir, "cat.ducklake")
	if err := cfg.check(); err != nil {
		t.Fatal(err)
	}
	e := &engine{cfg: cfg, lake: qi("lake"), hist: `"lake"."duckpipe".load_history`, root: dir,
		down: map[string]bool{}, pipes: map[string]pipeState{}, poke: make(chan struct{}, 1)}
	e.duck = &duck{bin: bin, setup: []string{"SET TimeZone = 'UTC'", "LOAD ducklake",
		"ATTACH " + ql(cfg.Lake.Catalog) + " AS lake (DATA_PATH " + ql(filepath.Join(dir, "data")+"/") + ")"}}
	if _, err := e.duck.run(context.Background(), e.historyDDL()...); err != nil {
		t.Skipf("no DuckLake: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "home", "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	return e, cfg.Pipes[0], dir
}

// put writes a file for the pipe and returns it as the poll would list it.
func put(t *testing.T, dir, name, data string) object {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "home", "in", name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return object{Key: "in/" + name, ETag: name, Size: int64(len(data))}
}

// attempt is a file's history row (one attempt only), with its file_stats.
type attempt struct {
	Status, Error, Check string
	Rows                 int64
	FileStats            map[string]any
}

func attempts(t *testing.T, e *engine, key string) []attempt {
	t.Helper()
	res, err := e.duck.query(context.Background(), `SELECT status, rows, error, `+checkSQL+` AS "check", file_stats::VARCHAR AS fs FROM `+
		e.hist+` WHERE key = `+ql(key)+` ORDER BY loaded_at DESC`)
	if err != nil {
		t.Fatal(err)
	}
	var out []attempt
	for _, r := range res {
		a := attempt{Status: r.str("status"), Error: r.str("error"), Check: r.str("check"), Rows: r.int("rows")}
		json.Unmarshal([]byte(r.str("fs")), &a.FileStats)
		out = append(out, a)
	}
	return out
}

func tableRows(t *testing.T, e *engine) int64 {
	t.Helper()
	res, err := e.duck.query(context.Background(), "SELECT count(*) AS n FROM lake.raw.orders")
	if err != nil {
		t.Fatal(err)
	}
	return res[0].int("n")
}

// A batch of CSVs: the clean ones load with both stats equal, the bad ones
// fail on their own, before the load, and nothing of them is in the table.
func TestStatsLoadCSV(t *testing.T) {
	e, p, dir := lakeEngine(t, "header = true, all_varchar = true")
	ctx := context.Background()
	good := put(t, dir, "good.csv", "id,where,note\n1,\"HIGH ROAD, ICKENHAM\",\"multi\nline\"\n2,PARK ROAD,\n")
	broken := put(t, dir, "broken.csv", "id,where,note\n1,PARK ROAD,x\n2,unquoted\nnewline,y\n")
	semi := put(t, dir, "semi.csv", "id;where;note\n1;a;b\n") // no delim in the options: 1 field for Go, 3 columns for DuckDB
	if err := e.ensureTable(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := e.loadBatch(ctx, p, []object{good, broken, semi}, nil); err != nil {
		t.Fatal(err)
	}

	a := attempts(t, e, good.Key)
	if len(a) != 1 || a[0].Status != "loaded" || a[0].Check != "ok" || a[0].Rows != 2 {
		t.Fatalf("good: %+v", a)
	}
	if fs := a[0].FileStats; fs["records"] != 2.0 || fs["fields"] != 3.0 || fs["columns"] != 3.0 {
		t.Errorf("good: file_stats %v", fs)
	}

	for _, c := range []struct {
		o    object
		want string
	}{{broken, "check: csv: record on line 3: wrong number of fields"}, {semi, "check: columns: 1 fields per record"}} {
		a := attempts(t, e, c.o.Key)
		if len(a) != 1 || a[0].Status != "failed" || a[0].Check != "failed" || !strings.HasPrefix(a[0].Error, c.want) {
			t.Errorf("%s: %+v, want error %q", c.o.Key, a, c.want)
		}
	}
	if n := tableRows(t, e); n != 2 {
		t.Errorf("table has %d rows, want the good file's 2", n)
	}

	// Rows that disappear from the table later are found by verify; so are
	// rows no history row loaded.
	if d, err := e.verify(ctx, ""); err != nil || len(d) != 0 {
		t.Fatalf("verify = %v, %v; want nothing", d, err)
	}
	if _, err := e.duck.run(ctx, "DELETE FROM lake.raw.orders WHERE record->>'id' = '1'",
		"INSERT INTO lake.raw.orders VALUES ('{}', 'elsewhere.csv', now())"); err != nil {
		t.Fatal(err)
	}
	d, err := e.verify(ctx, "orders")
	if err != nil || len(d) != 2 || !slices.Contains(d, drift{"orders", "elsewhere.csv", 0, 1}) ||
		!slices.Contains(d, drift{"orders", e.url(p, good), 2, 1}) {
		t.Errorf("verify = %+v, %v", d, err)
	}
}

// A file whose rows in the table aren't its records fails inside the load's
// transaction: its rows are deleted again, the other files of the batch
// stay, and its count is saved.
func TestStatsLoadLakeCheck(t *testing.T) {
	e, p, dir := lakeEngine(t, "")
	ctx := context.Background()
	if err := e.ensureTable(ctx, p); err != nil {
		t.Fatal(err)
	}
	var objs []object
	for _, name := range []string{"a.parquet", "b.parquet"} {
		path := filepath.Join(dir, "home", "in", name)
		if _, err := e.duck.run(ctx, "COPY (SELECT range AS id FROM range(5)) TO "+ql(path)); err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(path)
		objs = append(objs, object{Key: "in/" + name, ETag: name, Size: st.Size()})
	}
	fs, err := e.fileStats(ctx, p, objs)
	if err != nil {
		t.Fatal(err)
	}
	if s := fs["in/a.parquet"]; s.problem != "" || *s.Records != 5 {
		t.Fatalf("a: %+v", s)
	}
	six := int64(6)
	fs["in/b.parquet"].Records = &six // as if a row went missing on the way in
	if err := e.loadBatch(ctx, p, objs, fs); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"in/a.parquet": "ok", "in/b.parquet": "failed"} {
		a := attempts(t, e, key)
		if len(a) != 1 || a[0].Check != want {
			t.Errorf("%s: %+v, want check %s", key, a, want)
			continue
		}
		if want == "failed" && (a[0].Error != "check: rows: 6 in the file (Parquet footer), in the table 5" || a[0].FileStats["records"] != 6.0) {
			t.Errorf("%s: %+v", key, a[0])
		}
	}
	if n := tableRows(t, e); n != 5 {
		t.Errorf("table has %d rows, want a's 5", n)
	}
}
