package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// historyValues returns one row of run_history as a VALUES tuple. status,
// rows, errMsg, finished and snapshot are SQL expressions, so a script can
// fill them in from what it computed.
func (l *lake) historyValues(o runOptions, out *outcome, status, rows, errMsg, finished, snapshot string) string {
	jobs := make([]string, len(o.jobs))
	for i, j := range o.jobs {
		jobs[i] = ql(j)
	}
	return "(" + strings.Join([]string{
		ql(o.runID), "[" + strings.Join(jobs, ", ") + "]::VARCHAR[]", ql(l.p.target.name), ql(out.kind), ql(out.name),
		ql(out.relation), sqlOrNull(out.materialized), status, rows, errMsg,
		sqlTime(out.started), finished, snapshot,
	}, ", ") + ")"
}

// writeHistory writes the rows of the outcomes (and their tests) that are
// not written yet, in one transaction.
func (l *lake) writeHistory(ctx context.Context, o runOptions, outs []*outcome) error {
	var values []string
	add := func(out *outcome) {
		if out.logged {
			return
		}
		rows, snapshot := "NULL", "NULL"
		if out.rows >= 0 {
			rows = fmt.Sprint(out.rows)
		}
		if out.snapshot > 0 {
			snapshot = fmt.Sprint(out.snapshot)
		}
		values = append(values, l.historyValues(o, out, ql(out.status), rows, sqlOrNull(out.err), sqlTime(out.finished), snapshot))
	}
	for _, out := range outs {
		add(out)
		for _, t := range out.tests {
			add(t)
		}
	}
	if len(values) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var s script
	s.add("INSERT INTO " + l.hist + " VALUES " + strings.Join(values, ", "))
	_, err := l.run(ctx, &s)
	return err
}

func sqlOrNull(s string) string {
	if s == "" {
		return "NULL"
	}
	return ql(s)
}

func sqlTime(t time.Time) string {
	return ql(t.UTC().Format(time.RFC3339Nano)) + "::TIMESTAMPTZ"
}

// history prints the latest rows of run_history, newest first: for one
// model (with its tests) or test, if name is set.
func (l *lake) history(ctx context.Context, w io.Writer, name string, limit int) error {
	where := ""
	if name != "" {
		// A model's rows, and those of the tests that check it.
		where = " WHERE name = " + ql(name)
		if m := l.p.models[name]; m != nil {
			where += " OR kind = 'test' AND list_contains(string_split(relation, ','), " + ql(l.p.relation(m)) + ")"
		}
	}
	rows, err := l.query(ctx, `SELECT strftime(started_at AT TIME ZONE 'UTC', '%Y-%m-%d %H:%M:%S') AS started, run_id, kind, name, status,
		rows, round(epoch(finished_at - started_at), 2) AS seconds, coalesce(error, '') AS error
		FROM `+l.hist+where+` ORDER BY started_at DESC, kind, name LIMIT `+fmt.Sprint(limit))
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STARTED (UTC)\tRUN\tKIND\tNAME\tSTATUS\tROWS\tSECONDS\tERROR")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.str("started"), r.str("run_id"), r.str("kind"), r.str("name"),
			r.str("status"), dash(r.str("rows")), r.str("seconds"), firstLine(r.str("error")))
	}
	return tw.Flush()
}

// orphans returns the tables and views in the schemas the project builds in
// that no model builds.
func (l *lake) orphans(ctx context.Context) ([]string, error) {
	existing, err := l.relations(ctx)
	if err != nil {
		return nil, err
	}
	schemas, built := map[string]bool{}, map[string]bool{}
	for _, m := range l.p.models {
		schemas[l.p.target.schema(m.cfg.Schema)] = true
		built[l.p.relation(m)] = true
	}
	var out []string
	for _, rel := range sortedKeys(existing) {
		schema, _, _ := strings.Cut(rel, ".")
		if schemas[schema] && !built[rel] {
			out = append(out, rel+" ("+existing[rel]+")")
		}
	}
	return out, nil
}
