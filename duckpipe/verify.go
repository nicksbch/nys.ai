package main

// verify checks, after a load, that the tables still hold what the load
// history says was loaded into them: per file, the rows of its "loaded"
// history rows against its rows in the table now. A table dropped or
// emptied, or a history cleared while its table stayed, shows up as files
// that drifted.

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"text/tabwriter"
	"time"
)

// driftLimit is how many drifted files the page and list_pipes show at most.
const driftLimit = 50

// drift is a file whose rows in its table aren't the rows its load history
// says were loaded.
type drift struct {
	Pipe    string `json:"pipe"`
	File    string `json:"file"`
	History int64  `json:"history"` // rows loaded, by the load history
	Table   int64  `json:"table"`   // rows of the file in the table now
}

// verify returns the files that drifted, of every pipe or only one, by
// pipe and file.
func (e *engine) verify(ctx context.Context, only string) ([]drift, error) {
	// Pipes loading into the same table are checked together.
	byTarget := map[string][]*pipe{}
	var targets []string
	for _, p := range e.cfg.Pipes {
		if only != "" && p.Name != only {
			continue
		}
		t := e.targetName(p)
		if byTarget[t] == nil {
			targets = append(targets, t)
		}
		byTarget[t] = append(byTarget[t], p)
	}
	if only != "" && len(targets) == 0 {
		return nil, fmt.Errorf("there is no pipe %q", only)
	}

	var out []drift
	for _, t := range targets {
		ps := byTarget[t]
		p := ps[0]
		res, err := e.duck.query(ctx, "SELECT count(*) AS n FROM duckdb_tables() WHERE database_name = "+ql(e.cfg.Lake.Name)+
			" AND schema_name = "+ql(p.schema)+" AND table_name = "+ql(p.table))
		if err != nil {
			return nil, err
		}
		table := "SELECT NULL::VARCHAR AS _file, 0::BIGINT AS n WHERE false" // a table that is gone holds no rows
		if res[0].int("n") > 0 {
			table = "SELECT _file, count(*) AS n FROM " + e.target(p) + " GROUP BY _file"
		}
		names := make([]string, len(ps))
		for i, p := range ps {
			names[i] = ql(p.Name)
		}
		// Histories from before target was recorded count for the pipe's table.
		hist := `SELECT pipe, ` + e.urlSQL("bucket", "key") + ` AS file, sum(rows)::BIGINT AS want FROM ` + e.hist + `
			WHERE pipe IN (` + strings.Join(names, ", ") + `) AND status = 'loaded' AND coalesce(target, ` + ql(t) + `) = ` + ql(t) + `
			GROUP BY ALL`
		rows, err := e.duck.query(ctx, `SELECT h.pipe, coalesce(h.file, t._file) AS file, coalesce(h.want, 0) AS want, coalesce(t.n, 0) AS got
			FROM (`+hist+`) h FULL JOIN (`+table+`) t ON t._file = h.file
			WHERE coalesce(h.want, 0) <> coalesce(t.n, 0)
			ORDER BY file`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			pipe := r.str("pipe")
			if pipe == "" { // rows in the table that no history row loaded
				pipe = p.Name
			}
			out = append(out, drift{Pipe: pipe, File: r.str("file"), History: r.int("want"), Table: r.int("got")})
		}
	}
	return out, nil
}

// checkTables runs verify for every pipe, keeps what it found for the page,
// and publishes each pipe's result to relay as "duckpipe.verify", "ok" or
// "failed".
func (e *engine) checkTables(ctx context.Context) {
	start := time.Now()
	found, err := e.verify(ctx, "")
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("verify: %v", err)
		e.publish("verify", "failed", "", map[string]any{"name": "verify"}, err)
		return
	}
	now := time.Now()
	e.mu.Lock()
	e.verified, e.drifted = &now, found
	e.mu.Unlock()

	byPipe := map[string][]drift{}
	for _, d := range found {
		byPipe[d.Pipe] = append(byPipe[d.Pipe], d)
	}
	for _, p := range e.cfg.Pipes {
		ds := byPipe[p.Name]
		var err error
		if len(ds) > 0 {
			err = fmt.Errorf("%d file(s) don't match the load history in %s, e.g. %s: %d rows loaded, %d in the table",
				len(ds), e.targetName(p), ds[0].File, ds[0].History, ds[0].Table)
			log.Printf("%s: %v", p.Name, err)
		}
		e.publish("verify", result(err), "", map[string]any{"name": p.Name, "pipe": p.Name, "table": e.targetName(p),
			"files": len(ds)}, err)
	}
	log.Printf("verify: %d file(s) drifted, in %s", len(found), time.Since(start).Round(time.Millisecond))
}

// printVerify runs verify and lists the files that drifted.
func (e *engine) printVerify(ctx context.Context, w io.Writer, pipe string) error {
	found, err := e.verify(ctx, pipe)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Fprintln(w, "every file's rows match the load history")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PIPE\tFILE\tLOADED\tIN TABLE")
	for _, d := range found {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", d.Pipe, d.File, d.History, d.Table)
	}
	tw.Flush()
	return fmt.Errorf("%d file(s) don't match the load history; reload their tables (README, \"Reload a table\")", len(found))
}
