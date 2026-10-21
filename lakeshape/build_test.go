package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLake copies testdata/shop to a temporary directory with a lake of its
// own (SQLite catalog, data on local disk), with raw source tables. It needs
// DuckDB 1.5+: set LAKESHAPE_TEST_DUCKDB to its CLI.
func testLake(t *testing.T) (dir string, open func() *lake) {
	t.Helper()
	bin := os.Getenv("LAKESHAPE_TEST_DUCKDB")
	if bin == "" {
		t.Skip("set LAKESHAPE_TEST_DUCKDB to a duckdb 1.5+ CLI to run this test")
	}
	tmp := t.TempDir()
	dir = filepath.Join(tmp, "shop")
	if err := os.CopyFS(dir, os.DirFS("testdata/shop")); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "project.yaml"))
	lines := strings.Split(string(cfg), "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "  catalog:"):
			lines[i] = "  catalog: ducklake:sqlite:" + filepath.Join(tmp, "catalog.sqlite")
		case strings.HasPrefix(l, "  data_path:"):
			lines[i] = "  data_path: " + filepath.Join(tmp, "data") + "/\nthreads: 1"
		}
	}
	os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(strings.Join(lines, "\n")), 0o644)

	open = func() *lake {
		t.Helper()
		p, err := loadProject(dir, dev, nil)
		if err != nil {
			t.Fatal(err)
		}
		l, err := openLake(context.Background(), p, lakeOptions{duckdb: bin})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	l := open()
	sqlExec(t, l,
		"CREATE SCHEMA raw",
		`CREATE TABLE raw.orders AS SELECT * FROM (VALUES
			(1, 10, 'placed', 1000, TIMESTAMPTZ '2026-10-01 10:00:00+00'),
			(2, 11, 'shipped', 2550, TIMESTAMPTZ '2026-10-01 12:00:00+00'),
			(3, 10, 'placed', 500, TIMESTAMPTZ '2026-10-02 09:00:00+00')) v(order_id, customer_id, status, amount_cents, ordered_at)`,
		"CREATE TABLE raw.customers AS SELECT * FROM (VALUES (10, 'Ada'), (11, 'Bo')) v(id, name)")
	return dir, open
}

func sqlExec(t *testing.T, l *lake, stmts ...string) []row {
	t.Helper()
	rows, err := l.query(context.Background(), append(stmts, "SELECT 1")...)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func count(t *testing.T, l *lake, rel string) int64 {
	t.Helper()
	rows, err := l.query(context.Background(), "SELECT count(*) AS n FROM "+rel)
	if err != nil {
		t.Fatal(err)
	}
	return rows[0].int("n")
}

func statuses(res *runResult) map[string]string {
	out := map[string]string{}
	for _, o := range res.outcomes {
		out[o.name] = o.status
		for _, t := range o.tests {
			out[t.name] = t.status
		}
	}
	return out
}

func TestBuild(t *testing.T) {
	_, open := testLake(t)
	ctx := context.Background()
	l := open()
	o := runOptions{out: io.Discard}

	res, err := l.build(ctx, l.p.order, o)
	if err != nil {
		t.Fatal(err)
	}
	st := statuses(res)
	if !res.ok() || st["orders"] != statusSuccess || st["revenue_matches_orders"] != statusSuccess || st["unique_orders_order_id"] != statusSuccess {
		t.Fatalf("first run: %v", st)
	}
	if n := count(t, l, "dev_nicolas__marts.orders"); n != 3 {
		t.Errorf("orders has %d rows, want 3", n)
	}

	// A value accepted_values doesn't allow: orders rolls back, its
	// downstream is skipped.
	sqlExec(t, l, "INSERT INTO raw.orders VALUES (4, 10, 'lost', 700, TIMESTAMPTZ '2026-10-03 09:00:00+00')")
	res, err = l.build(ctx, l.p.order, o)
	if err != nil {
		t.Fatal(err)
	}
	st = statuses(res)
	if res.ok() || st["orders"] != statusError || st["accepted_values_orders_status"] != statusError ||
		st["revenue"] != statusSkipped || st["stg_orders"] != statusSuccess {
		t.Fatalf("bad row: %v", st)
	}
	if n := count(t, l, "dev_nicolas__marts.orders"); n != 3 {
		t.Errorf("after a failed test, orders has %d rows, want the 3 from before", n)
	}
	rows, _ := l.query(ctx, "SELECT status FROM dev_nicolas__lakeshape.run_history WHERE run_id = "+ql(res.runID)+" AND name IN ('orders', 'revenue') ORDER BY name")
	if len(rows) != 2 || rows[0].str("status") != statusError || rows[1].str("status") != statusSkipped {
		t.Errorf("history of the failed run: %v", rows)
	}

	// Fixed: the incremental run adds just the new row.
	sqlExec(t, l, "UPDATE raw.orders SET status = 'returned' WHERE order_id = 4")
	res, err = l.build(ctx, l.p.order, o)
	if err != nil || !res.ok() {
		t.Fatalf("fixed: %v %v", err, statuses(res))
	}
	for _, out := range res.outcomes {
		if out.name == "orders" && (out.rows != 1 || out.snapshot == 0) {
			t.Errorf("incremental orders wrote %d rows (snapshot %d), want 1", out.rows, out.snapshot)
		}
	}
	if n := count(t, l, "dev_nicolas__marts.orders"); n != 4 {
		t.Errorf("orders has %d rows, want 4", n)
	}

	res, err = l.test(ctx, l.p.order, o)
	if err != nil || !res.ok() || len(res.outcomes) != 5 {
		t.Fatalf("test: %v %v", err, statuses(res))
	}
}

func TestDevNeedsUpstream(t *testing.T) {
	_, open := testLake(t)
	l := open()
	_, err := l.build(context.Background(), []*model{l.p.models["orders"]}, runOptions{out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "ref('stg_orders') → dev_nicolas__staging.stg_orders does not exist") {
		t.Errorf("error = %v", err)
	}
}
