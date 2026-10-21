package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// Statuses of a model or test in a run.
const (
	statusSuccess = "success"
	statusWarn    = "warn"  // built; a warn test failed (or, for a test, it failed)
	statusError   = "error" // failed; a model rolled back
	statusSkipped = "skipped"
)

// attempts is how often a model is built when its commit conflicts with
// another writer's.
const attempts = 3

type runOptions struct {
	runID       string
	jobs        []string
	fullRefresh bool
	out         io.Writer // progress, one line per model and failed test
	// notify, if set, is told about the run and each model as they happen.
	notify func(topic, typ string, data map[string]any)
}

// outcome is what happened to one model or test.
type outcome struct {
	kind         string // model or test
	name         string
	relation     string // built, or the model a test checks
	materialized string
	status       string
	rows         int64 // written (models) or failing (tests); -1 if unknown
	err          string
	started      time.Time
	finished     time.Time
	snapshot     int64
	tests        []*outcome // a model's own tests
	logged       bool       // its history row is written
}

type runResult struct {
	runID    string
	outcomes []*outcome // models and tests on several models, in the order they finished
	started  time.Time
	finished time.Time
}

// ok reports whether every model and test succeeded or only warned.
func (r *runResult) ok() bool {
	for _, o := range r.outcomes {
		if o.status == statusError || o.status == statusSkipped {
			return false
		}
		for _, t := range o.tests {
			if t.status == statusError {
				return false
			}
		}
	}
	return true
}

func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// node is a model, or a test on several models, to run.
type node struct {
	m *model
	t *test
}

func (n node) key() string {
	if n.m != nil {
		return "model:" + n.m.name
	}
	return "test:" + n.t.id
}

// build builds the selected models in DAG order, up to p.Threads at a time,
// with their tests. A model that fails rolls back, and everything downstream
// of it is skipped.
func (l *lake) build(ctx context.Context, sel []*model, o runOptions) (*runResult, error) {
	p := l.p
	if o.runID == "" {
		o.runID = newRunID()
	}
	res := &runResult{runID: o.runID, started: time.Now()}
	selected := map[string]bool{}
	for _, m := range sel {
		selected[m.name] = true
	}

	// Every schema first, one after another, so parallel models don't race to
	// create them; then what exists, for is_incremental().
	var s script
	schemas := map[string]bool{}
	for _, m := range sel {
		schema := p.target.schema(m.cfg.Schema)
		if err := p.target.checkWrite(schema); err != nil {
			return nil, err
		}
		if !schemas[schema] {
			schemas[schema] = true
			s.add("CREATE SCHEMA IF NOT EXISTS " + schema)
		}
	}
	if _, err := l.run(ctx, &s); err != nil {
		return nil, fmt.Errorf("creating schemas: %w", err)
	}
	existing, err := l.relations(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the lake's tables: %w", err)
	}

	// Upstream models that are not selected must already exist.
	failed := map[string]string{} // model -> why it fails before it starts
	var missing []string
	for _, m := range sel {
		for _, d := range m.deps() {
			if dep := p.models[d]; !selected[d] && existing[p.relation(dep)] == "" {
				how := "ref('" + d + "')"
				if !slices.Contains(m.refs, d) {
					how = "its relationships test against " + d
				}
				missing = append(missing, fmt.Sprintf("%s: %s → %s does not exist", m.name, how, p.relation(dep)))
				failed[m.name] = fmt.Sprintf("upstream %s (%s) has not been built", d, p.relation(dep))
			}
		}
	}
	if len(missing) > 0 && p.target.name == "dev" {
		return nil, fmt.Errorf("upstream models are not built in dev; build them too, e.g. with +%s:\n  %s", missing[0][:strings.Index(missing[0], ":")], strings.Join(missing, "\n  "))
	}

	incremental := map[string]bool{}
	sqls := map[string]string{}
	for _, m := range sel {
		incremental[m.name] = m.cfg.Materialized == "incremental" && existing[p.relation(m)] == "table" && !o.fullRefresh
		sql, err := p.compile(m, incremental[m.name])
		if err != nil {
			failed[m.name] = err.Error()
		}
		sqls[m.name] = sql
	}

	// Tests on several models run once their selected models are built, and
	// the models downstream of all of those wait for them.
	ancestors := map[string]map[string]bool{}
	for _, m := range p.order { // dependencies first, so theirs are known
		a := map[string]bool{}
		for _, d := range m.deps() {
			a[d] = true
			for x := range ancestors[d] {
				a[x] = true
			}
		}
		ancestors[m.name] = a
	}
	upstreamOf := func(t *test, m *model) bool {
		for _, tm := range t.models {
			if !ancestors[m.name][tm] {
				return false
			}
		}
		return true
	}
	var nodes []node
	for _, m := range sel {
		nodes = append(nodes, node{m: m})
	}
	gates := map[string][]*test{}
	for _, t := range p.tests {
		if t.own() || !slices.ContainsFunc(t.models, func(m string) bool { return selected[m] }) {
			continue
		}
		nodes = append(nodes, node{t: t})
		for _, m := range t.models {
			gates[m] = append(gates[m], t)
		}
	}

	fmt.Fprintf(o.out, "run %s (%s): %d models, %d threads\n", o.runID, p.target.name, len(sel), p.Threads)
	l.notify(o, "run", "started", map[string]any{"jobs": o.jobs, "models": len(sel), "target": p.target.name})

	done := map[string]*outcome{}
	finish := func(n node, out *outcome) {
		done[n.key()] = out
		res.outcomes = append(res.outcomes, out)
		l.report(o, out)
	}
	// decide says whether n can start now, or must be skipped (with why).
	decide := func(n node) (ready bool, skip string) {
		var after []string // models n runs after
		if n.m != nil {
			after = n.m.deps()
		} else {
			after = n.t.models
		}
		ready = true
		for _, d := range after {
			if !selected[d] {
				continue
			}
			out := done["model:"+d]
			switch {
			case out == nil:
				ready = false
				continue
			case out.status == statusError:
				return false, "upstream " + d + " failed"
			case out.status == statusSkipped:
				return false, "upstream " + d + " was skipped"
			}
			if n.m == nil {
				continue
			}
			for _, t := range gates[d] {
				if !upstreamOf(t, n.m) {
					continue // the test waits for n itself, or its downstream
				}
				switch out := done["test:"+t.id]; {
				case out == nil:
					ready = false
				case out.status == statusError:
					return false, "test " + t.id + " failed"
				}
			}
		}
		return ready, ""
	}

	type finished struct {
		n   node
		out *outcome
	}
	results := make(chan finished)
	var wg sync.WaitGroup
	running := 0
	pending := nodes
	for len(pending) > 0 || running > 0 {
		var still []node
		for _, n := range pending {
			if ctx.Err() != nil {
				finish(n, skipped(p, n, "cancelled"))
				continue
			}
			if n.m != nil && failed[n.m.name] != "" {
				out := newOutcome(p, n)
				out.status, out.err, out.finished = statusError, failed[n.m.name], time.Now()
				finish(n, out)
				continue
			}
			ready, skip := decide(n)
			switch {
			case skip != "":
				finish(n, skipped(p, n, skip))
			case ready && running < p.Threads:
				running++
				wg.Add(1)
				go func(n node) {
					defer wg.Done()
					var out *outcome
					if n.m != nil {
						out = l.buildModel(ctx, n.m, sqls[n.m.name], incremental[n.m.name], existing[p.relation(n.m)], o)
					} else {
						out = l.runTest(ctx, n.t)
					}
					results <- finished{n, out}
				}(n)
			default:
				still = append(still, n)
			}
		}
		pending = still
		if running == 0 {
			if len(pending) > 0 { // can't happen: the DAG has no cycles
				return nil, fmt.Errorf("%d models can never start", len(pending))
			}
			break
		}
		r := <-results
		running--
		finish(r.n, r.out)
	}
	wg.Wait()
	res.finished = time.Now()

	// Rows the scripts could not write: failures roll back with their model.
	if err := l.writeHistory(context.WithoutCancel(ctx), o, res.outcomes); err != nil {
		fmt.Fprintf(o.out, "warning: writing run history: %v\n", err)
	}
	l.summary(o, res)
	return res, nil
}

func newOutcome(p *project, n node) *outcome {
	if n.m != nil {
		return &outcome{kind: "model", name: n.m.name, relation: p.relation(n.m), materialized: n.m.cfg.Materialized, rows: -1, started: time.Now()}
	}
	var rels []string
	for _, m := range n.t.models {
		rels = append(rels, p.relation(p.models[m]))
	}
	return &outcome{kind: "test", name: n.t.id, relation: strings.Join(rels, ","), rows: -1, started: time.Now()}
}

func skipped(p *project, n node, why string) *outcome {
	out := newOutcome(p, n)
	out.status, out.err, out.finished = statusSkipped, why, out.started
	return out
}

// buildModel builds m in one transaction with its own tests, retrying when the
// commit conflicts with another writer.
func (l *lake) buildModel(ctx context.Context, m *model, sql string, incremental bool, existing string, o runOptions) *outcome {
	for attempt := 1; ; attempt++ {
		out, err := l.buildOnce(ctx, m, sql, incremental, existing, o)
		if err == nil || !conflict(err) || attempt == attempts || ctx.Err() != nil {
			return out
		}
		select {
		case <-ctx.Done():
			return out
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
}

func (l *lake) buildOnce(ctx context.Context, m *model, sql string, incremental bool, existing string, o runOptions) (*outcome, error) {
	p := l.p
	out := newOutcome(p, node{m: m})
	rel := p.relation(m)
	schema, table, _ := strings.Cut(rel, ".")
	fail := func(msg string) *outcome {
		out.status, out.err, out.finished = statusError, msg, time.Now()
		return out
	}
	if err := p.target.checkWrite(schema); err != nil {
		return fail(err.Error()), nil
	}

	// The model's SQL goes inside parentheses, so it can only be one query.
	body := "SELECT * FROM (\n" + strings.TrimRight(strings.TrimSpace(sql), ";") + "\n)"
	var s script
	s.add("BEGIN", fmt.Sprintf("CALL ducklake_set_commit_message('%s', 'lakeshape', %s)", lakeName, ql("run "+o.runID+": "+m.name)))
	rows := "NULL"
	switch {
	case m.cfg.Materialized == "view":
		if existing == "table" {
			s.add("DROP TABLE " + rel)
		}
		s.add("CREATE OR REPLACE VIEW " + rel + " AS " + body)
	case !incremental: // a table, or an incremental model's first build
		if existing == "view" {
			s.add("DROP VIEW " + rel)
		}
		s.add("CREATE OR REPLACE TABLE " + rel + " AS " + body)
		rows = "(SELECT count(*) FROM " + rel + ")"
	default:
		cols := func(db, schemaCond, table string) string {
			return "(SELECT list(column_name || ' ' || data_type ORDER BY column_index) FROM duckdb_columns() WHERE database_name = '" + db + "'" + schemaCond + " AND table_name = " + ql(table) + ")"
		}
		s.add("CREATE TEMP TABLE __ls_new AS "+body,
			"SET VARIABLE __ls_new_cols = "+cols("temp", "", "__ls_new"),
			"SET VARIABLE __ls_old_cols = "+cols(lakeName, " AND schema_name = "+ql(schema), table),
			`SELECT error('the columns changed from ' || array_to_string(getvariable('__ls_old_cols'), ', ') || ' to ' || array_to_string(getvariable('__ls_new_cols'), ', ') || '; build it again with -full-refresh')
				WHERE getvariable('__ls_new_cols') IS DISTINCT FROM getvariable('__ls_old_cols')`)
		if m.cfg.IncrementalStrategy == "delete+insert" {
			var on []string
			for _, k := range m.cfg.UniqueKey {
				on = append(on, "n."+qi(k)+" = "+qi(table)+"."+qi(k))
			}
			s.add("DELETE FROM " + rel + " WHERE EXISTS (SELECT 1 FROM temp.__ls_new n WHERE " + strings.Join(on, " AND ") + ")")
		}
		s.add("INSERT INTO " + rel + " SELECT * FROM temp.__ls_new")
		rows = "(SELECT count(*) FROM temp.__ls_new)"
	}
	s.add("SET VARIABLE __ls_rows = " + rows)
	var counts, errorTests []string
	for i, t := range m.tests {
		v := fmt.Sprintf("__ls_t%d", i)
		s.add("SET VARIABLE " + v + " = (SELECT count(*) FROM (\n" + t.sql + "\n))")
		counts = append(counts, "getvariable('"+v+"')")
		if t.severity == "error" {
			errorTests = append(errorTests, "getvariable('"+v+"') > 0")
		}
	}
	s.out("counts", "SELECT getvariable('__ls_rows') AS rows, ["+strings.Join(counts, ", ")+"]::BIGINT[] AS tests")
	if len(errorTests) > 0 {
		s.add("SELECT error('a test failed') WHERE " + strings.Join(errorTests, " OR "))
	}
	s.add("COMMIT")
	s.mark("committed")
	s.add("SET VARIABLE __ls_snap = (SELECT id FROM ducklake_last_committed_snapshot('" + lakeName + "'))")
	s.out("snapshot", "SELECT getvariable('__ls_snap')::BIGINT AS id")

	// The history rows, in their own transaction right after the commit.
	status := "'success'"
	var warnTests []string
	for i, t := range m.tests {
		if t.severity == "warn" {
			warnTests = append(warnTests, fmt.Sprintf("getvariable('__ls_t%d') > 0", i))
		}
	}
	if len(warnTests) > 0 {
		status = "CASE WHEN " + strings.Join(warnTests, " OR ") + " THEN 'warn' ELSE 'success' END"
	}
	hist := []string{l.historyValues(o, out, status, "getvariable('__ls_rows')", "NULL", "now()", "getvariable('__ls_snap')::BIGINT")}
	for i, t := range m.tests {
		v := fmt.Sprintf("getvariable('__ls_t%d')", i)
		tout := &outcome{kind: "test", name: t.id, relation: rel, started: out.started}
		hist = append(hist, l.historyValues(o, tout, "CASE WHEN "+v+" > 0 THEN '"+t.severity+"' ELSE 'success' END", v, "NULL", "now()", "NULL"))
	}
	s.add("INSERT INTO " + l.hist + " VALUES " + strings.Join(hist, ", "))
	s.mark("logged")

	r, err := l.run(ctx, &s)
	out.finished = time.Now()
	if c := r.rows["counts"]; len(c) == 1 {
		out.rows = c[0].int("rows")
		if c[0]["rows"] == nil {
			out.rows = -1
		}
		failedTests := c[0]["tests"].([]any)
		for i, t := range m.tests {
			n := row{"n": failedTests[i]}.int("n")
			tout := &outcome{kind: "test", name: t.id, relation: rel, rows: n, status: statusSuccess, started: out.started, finished: out.finished, logged: r.reached["logged"]}
			if n > 0 {
				tout.status = t.severity
				tout.err = fmt.Sprintf("%d failing rows", n)
			}
			out.tests = append(out.tests, tout)
		}
	}
	if r.reached["committed"] {
		out.status = statusSuccess
		for _, t := range out.tests {
			if t.status == statusWarn {
				out.status = statusWarn
			}
		}
		if s := r.rows["snapshot"]; len(s) == 1 {
			out.snapshot = s[0].int("id")
		}
		out.logged = r.reached["logged"]
		return out, nil
	}
	if ctx.Err() != nil {
		return fail("cancelled"), ctx.Err()
	}
	for _, t := range out.tests {
		if t.status == statusError {
			fail("test " + t.name + " failed: " + t.err + "; rolled back")
			return out, nil
		}
	}
	if err == nil {
		err = fmt.Errorf("the script ended before COMMIT")
	}
	return fail(err.Error()), err
}

// runTest runs a test on several models, after they are built.
func (l *lake) runTest(ctx context.Context, t *test) *outcome {
	out := newOutcome(l.p, node{t: t})
	rows, err := l.query(ctx, "SELECT count(*) AS n FROM (\n"+t.sql+"\n)")
	out.finished = time.Now()
	switch {
	case err != nil:
		out.status, out.err = statusError, err.Error()
	case rows[0].int("n") > 0:
		out.rows = rows[0].int("n")
		out.status, out.err = t.severity, fmt.Sprintf("%d failing rows", out.rows)
	default:
		out.rows, out.status = 0, statusSuccess
	}
	return out
}

// test runs the tests of the selected models against what is built, without
// building anything.
func (l *lake) test(ctx context.Context, sel []*model, o runOptions) (*runResult, error) {
	p := l.p
	if o.runID == "" {
		o.runID = newRunID()
	}
	res := &runResult{runID: o.runID, started: time.Now()}
	selected := map[string]bool{}
	for _, m := range sel {
		selected[m.name] = true
	}
	var tests []*test
	for _, t := range p.tests {
		if slices.ContainsFunc(t.models, func(m string) bool { return selected[m] }) {
			tests = append(tests, t)
		}
	}
	fmt.Fprintf(o.out, "test %s (%s): %d tests, %d threads\n", o.runID, p.target.name, len(tests), p.Threads)

	outs := make([]*outcome, len(tests))
	sem := make(chan struct{}, p.Threads)
	var wg sync.WaitGroup
	for i, t := range tests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outs[i] = l.runTest(ctx, t)
		}()
	}
	wg.Wait()
	for _, out := range outs {
		if ctx.Err() != nil && out.status == statusError {
			out.err = "cancelled"
		}
		res.outcomes = append(res.outcomes, out)
		l.report(o, out)
	}
	res.finished = time.Now()
	if err := l.writeHistory(context.WithoutCancel(ctx), o, res.outcomes); err != nil {
		fmt.Fprintf(o.out, "warning: writing run history: %v\n", err)
	}
	l.summary(o, res)
	return res, nil
}

func (l *lake) notify(o runOptions, kind, typ string, data map[string]any) {
	if o.notify != nil {
		o.notify("lakeshape."+kind, typ, data)
	}
}

// report prints one finished model or test, and tells notify about models.
func (l *lake) report(o runOptions, out *outcome) {
	dur := out.finished.Sub(out.started).Round(10 * time.Millisecond)
	info := ""
	if out.rows >= 0 && out.kind == "model" {
		info = fmt.Sprintf("%d rows", out.rows)
	}
	if out.err != "" {
		info = firstLine(out.err)
	}
	what := out.materialized
	if out.kind == "test" {
		what = "test " + out.name
	}
	fmt.Fprintf(o.out, "  %-8s %-40s %-12s %6s  %s\n", out.status, out.relation, what, dur, info)
	for _, t := range out.tests {
		if t.status != statusSuccess {
			fmt.Fprintf(o.out, "    %-6s test %s: %s\n", t.status, t.name, t.err)
		}
	}
	if out.kind != "model" {
		return
	}
	typ := map[string]string{statusSuccess: "ok", statusWarn: "warn", statusError: "failed", statusSkipped: "skipped"}[out.status]
	data := map[string]any{"model": out.name, "relation": out.relation, "materialized": out.materialized,
		"duration_ms": dur.Milliseconds()}
	if out.rows >= 0 {
		data["rows"] = out.rows
	}
	if out.err != "" {
		data["error"] = out.err
	}
	var failedTests []string
	for _, t := range out.tests {
		if t.status != statusSuccess {
			failedTests = append(failedTests, t.name)
		}
	}
	if failedTests != nil {
		data["tests"] = failedTests
	}
	l.notify(o, "model", typ, data)
}

func (l *lake) summary(o runOptions, res *runResult) {
	n := map[string]int{}
	var failedModels []string
	for _, out := range res.outcomes {
		n[out.status]++
		if out.status == statusError {
			failedModels = append(failedModels, out.name)
		}
	}
	dur := res.finished.Sub(res.started).Round(10 * time.Millisecond)
	fmt.Fprintf(o.out, "done in %s: %d ok, %d warn, %d error, %d skipped\n", dur, n[statusSuccess], n[statusWarn], n[statusError], n[statusSkipped])
	data := map[string]any{"jobs": o.jobs, "models": len(res.outcomes), "duration_ms": dur.Milliseconds()}
	typ := "ok"
	if !res.ok() {
		typ = "failed"
		data["error"] = fmt.Sprintf("%d error, %d skipped: %s", n[statusError], n[statusSkipped], strings.Join(failedModels, ", "))
	}
	l.notify(o, "run", typ, data)
}
