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

// duck runs SQL scripts with the duckdb CLI, so duckpipe itself needs no cgo
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
	Type string // e.g. "Conversion", "IO", "HTTP"
	Msg  string
}

func (e *sqlError) Error() string { return e.Msg }

const marker = "@@duckpipe-result@@"

// run executes stmts in one CLI process, stopping at the first error. It
// returns the rows of each statement, in order (nil for statements that
// return none).
func (d *duck) run(ctx context.Context, stmts ...string) ([][]row, error) {
	var script strings.Builder
	for _, s := range d.setup {
		script.WriteString(s + ";\n")
	}
	// Results are told apart by a marker printed before each statement; the
	// setup's own output (e.g. CREATE SECRET's) comes before the first one.
	for _, s := range stmts {
		script.WriteString(".print " + marker + "\n" + strings.TrimSpace(s) + ";\n")
	}

	cmd := exec.CommandContext(ctx, d.bin, "-json", "-bail", "-batch", "-init", "/dev/null", ":memory:")
	cmd.Stdin = strings.NewReader(script.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("%s: %w", d.bin, runErr)
		}
		// "Conversion Error: Could not convert ..." -> type "Conversion".
		typ := ""
		if head, _, ok := strings.Cut(firstLine(msg), ":"); ok && strings.HasSuffix(head, "Error") {
			typ = strings.TrimSpace(strings.TrimSuffix(head, "Error"))
		}
		return nil, &sqlError{Type: typ, Msg: msg}
	}

	sections := strings.Split(stdout.String(), marker+"\n")
	if len(sections) != len(stmts)+1 {
		return nil, fmt.Errorf("duckdb printed %d results for %d statements", len(sections)-1, len(stmts))
	}
	results := make([][]row, len(stmts))
	for i, sec := range sections[1:] {
		// A SELECT prints one JSON array; statements without results print
		// nothing.
		dec := json.NewDecoder(bufio.NewReader(strings.NewReader(sec)))
		dec.UseNumber()
		for dec.More() {
			var rows []row
			if err := dec.Decode(&rows); err != nil {
				return nil, fmt.Errorf("reading duckdb output: %w", err)
			}
			results[i] = append(results[i], rows...)
		}
	}
	return results, nil
}

// query runs stmts and returns the rows of the last one.
func (d *duck) query(ctx context.Context, stmts ...string) ([]row, error) {
	res, err := d.run(ctx, stmts...)
	if err != nil {
		return nil, err
	}
	return res[len(res)-1], nil
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

// Error types that are about the environment (network, S3, the catalog,
// memory) rather than the file being loaded.
var transientTypes = map[string]bool{
	"IO": true, "HTTP": true, "Network": true, "Connection": true,
	"TransactionContext": true, "Interrupt": true, "Out of Memory": true,
	"FATAL": true, "Fatal": true, "Internal": true, "Permission": true,
	"Missing Extension": true, "Extension Autoloading": true, "Invalid Configuration": true,
}

// transient reports whether err should be retried on the next poll instead of
// marking the file failed. Anything that isn't a DuckDB error (the CLI
// crashed, was killed, ...) counts as transient too.
func transient(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	var se *sqlError
	if !errors.As(err, &se) {
		return true
	}
	return se.Type == "" || transientTypes[se.Type]
}
