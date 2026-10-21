package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// duck runs SQL scripts with the duckdb CLI, so lakeshape itself needs no cgo
// and cross-compiles like the other tools. Every script starts with the same
// setup (settings, S3 secret, ATTACH of the lake), and runs in a fresh
// in-memory DuckDB that exits when the script ends: an unfinished transaction
// is rolled back with it.
type duck struct {
	bin   string
	setup []string
}

// row is one result row, as the CLI's JSON output gives it. Numbers are
// json.Number, timestamps strings.
type row map[string]any

// sqlError is an error reported by DuckDB, e.g. "Conversion Error: ...".
type sqlError struct {
	Type string // e.g. "Conversion", "IO", "TransactionContext"
	Msg  string
}

func (e *sqlError) Error() string { return e.Msg }

const marker = "@@lakeshape@@"

// script is a list of statements. Statements added with out have their rows
// returned under a name; the others' output is dropped.
type script struct {
	b strings.Builder
}

func (s *script) add(stmts ...string) {
	for _, st := range stmts {
		s.b.WriteString(strings.TrimSpace(st) + ";\n")
	}
}

// out adds a statement whose rows run returns under name.
func (s *script) out(name, stmt string) {
	s.b.WriteString(".print " + marker + name + "\n")
	s.add(stmt)
}

// mark adds a named point with no rows, to tell how far the script got.
func (s *script) mark(name string) {
	s.b.WriteString(".print " + marker + name + "\n")
}

// result is what a script printed: the rows of each out statement, and the
// names of every out and mark reached.
type result struct {
	rows    map[string][]row
	reached map[string]bool
}

// run executes s in one CLI process, stopping at the first error. The result
// holds what was printed before the error too.
func (d *duck) run(ctx context.Context, s *script) (*result, error) {
	var full strings.Builder
	for _, st := range d.setup {
		full.WriteString(st + ";\n")
	}
	full.WriteString(s.b.String())

	cmd := exec.CommandContext(ctx, d.bin, "-json", "-bail", "-batch", "-init", "/dev/null", ":memory:")
	cmd.Stdin = strings.NewReader(full.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	res := &result{rows: map[string][]row{}, reached: map[string]bool{}}
	// The setup's own output (e.g. CREATE SECRET's) comes before the first marker.
	sections := strings.Split(stdout.String(), marker)
	for _, sec := range sections[1:] {
		name, body, _ := strings.Cut(sec, "\n")
		res.reached[name] = true
		dec := json.NewDecoder(bufio.NewReader(strings.NewReader(body)))
		dec.UseNumber()
		for dec.More() {
			var rows []row
			if err := dec.Decode(&rows); err != nil {
				return res, fmt.Errorf("reading duckdb output: %w", err)
			}
			res.rows[name] = append(res.rows[name], rows...)
		}
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return res, fmt.Errorf("%s: %w", d.bin, runErr)
		}
		// "Conversion Error: Could not convert ..." -> type "Conversion".
		typ := ""
		if head, _, ok := strings.Cut(firstLine(msg), ":"); ok && strings.HasSuffix(head, "Error") {
			typ = strings.TrimSpace(strings.TrimSuffix(head, "Error"))
		}
		return res, &sqlError{Type: typ, Msg: msg}
	}
	return res, nil
}

// query runs stmts and returns the rows of the last one.
func (d *duck) query(ctx context.Context, stmts ...string) ([]row, error) {
	var s script
	s.add(stmts[:len(stmts)-1]...)
	s.out("q", stmts[len(stmts)-1])
	res, err := d.run(ctx, &s)
	if err != nil {
		return nil, err
	}
	return res.rows["q"], nil
}

func (r row) str(col string) string {
	switch v := r[col].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func (r row) int(col string) int64 {
	// HUGEINT (e.g. from sum()) comes as a string.
	v, _ := strconv.ParseInt(r.str(col), 10, 64)
	return v
}

// conflict reports whether err is a commit that failed because another
// writer got there first (or, with an SQLite catalog, held the lock). Such a
// model is rolled back and can be built again.
func conflict(err error) bool {
	var se *sqlError
	return errors.As(err, &se) && se.Type == "TransactionContext"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// ql quotes a SQL string literal.
func ql(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// qi quotes a SQL identifier.
func qi(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
