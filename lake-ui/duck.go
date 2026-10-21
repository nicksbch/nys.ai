package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// duck runs scripts with the duckdb CLI, so lake-ui needs no cgo and
// cross-compiles like the other tools. Every script starts with the same
// setup (settings, S3 secret, read-only ATTACH of the lake) and runs in a
// fresh in-memory DuckDB that exits when the script ends.
type duck struct {
	bin   string
	setup string // run before every script; it starts with ".output /dev/null"
}

// sqlError is an error reported by DuckDB, e.g. "Binder Error: ...".
type sqlError struct{ msg string }

func (e *sqlError) Error() string { return e.msg }

// newDuck installs the extensions lake-ui needs and prepares the setup that
// attaches the lake. The catalog's PostgreSQL credentials come from libpq's
// PG* env vars, the S3 ones from the AWS_* vars.
func newDuck(ctx context.Context, bin, catalog, endpoint, stateDir string) (*duck, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("DuckDB CLI not found (-duckdb): %w", err)
	}
	var pre []string
	if stateDir != "" {
		pre = append(pre,
			"SET extension_directory = "+ql(filepath.Join(stateDir, "extensions")),
			"SET temp_directory = "+ql(filepath.Join(stateDir, "tmp")),
		)
	}
	d := &duck{bin: path, setup: ".output /dev/null\n" + stmts(pre...)}
	// Install once, before anything is attached, so errors here say what is
	// missing. The first run downloads the extensions.
	if err := d.run(ctx, "INSTALL httpfs; INSTALL ducklake; INSTALL postgres;\n", io.Discard); err != nil {
		return nil, fmt.Errorf("installing DuckDB extensions: %w", err)
	}

	// The script goes to duckdb on stdin, so the secret never shows up in a
	// process list, and the secret is temporary: nothing is saved.
	region := envOr("AWS_REGION", envOr("AWS_DEFAULT_REGION", "garage"))
	secret := []string{"TYPE s3", "REGION " + ql(region), "URL_STYLE 'path'"}
	if id := os.Getenv("AWS_ACCESS_KEY_ID"); id != "" {
		secret = append(secret, "KEY_ID "+ql(id), "SECRET "+ql(os.Getenv("AWS_SECRET_ACCESS_KEY")))
	}
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("endpoint %q: want a URL like http://host:3900", endpoint)
		}
		secret = append(secret, "ENDPOINT "+ql(u.Host), fmt.Sprintf("USE_SSL %t", u.Scheme == "https"))
	}
	d.setup += stmts(
		"LOAD httpfs", "LOAD ducklake", "LOAD postgres",
		"CREATE TEMPORARY SECRET lake_ui_s3 ("+strings.Join(secret, ", ")+")",
		"ATTACH "+ql(catalog)+" AS "+lakeAlias+" (READ_ONLY)",
		"USE "+lakeAlias,
	)
	return d, nil
}

// run executes the setup and then script in one CLI process, stopping at the
// first error. The setup's output is discarded; script turns output back on
// with ".output" when it wants the CLI's results on stdout.
func (d *duck) run(ctx context.Context, script string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, d.bin, "-bail", "-batch", "-init", "/dev/null", ":memory:")
	cmd.Stdin = strings.NewReader(d.setup + script)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("%s: %w", d.bin, runErr)
		}
		return &sqlError{cleanError(msg)}
	}
	return nil
}

// cleanError drops the "LINE 1: SELECT * FROM query(..." excerpt, which shows
// lake-ui's wrapper rather than the SQL that was typed, and explains the
// one-statement rule that query() enforces.
func cleanError(msg string) string {
	if i := strings.Index(msg, "\nLINE "); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	if strings.Contains(msg, "Expected a single SELECT statement") {
		msg += " (lake-ui runs one read-only statement at a time: SELECT, FROM, DESCRIBE, SHOW, SUMMARIZE, ...)"
	}
	return msg
}

// wrap turns the SQL typed in the page into "query('...')", a table that
// can be selected from. DuckDB's query() runs exactly one statement that
// returns rows and refuses anything else, and since the SQL is inside a
// string literal, the CLI can't read a line of it as a dot command.
func wrap(sql string) (string, error) {
	sql = strings.TrimSpace(sql)
	for strings.HasSuffix(sql, ";") {
		sql = strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	}
	if sql == "" {
		return "", fail(http.StatusBadRequest, "nothing to run")
	}
	if strings.ContainsRune(sql, 0) {
		return "", fail(http.StatusBadRequest, "the query contains a NUL character")
	}
	return "query(" + ql(sql) + ")", nil
}

// ql quotes s as an SQL string literal.
func ql(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func stmts(ss ...string) string {
	var b strings.Builder
	for _, s := range ss {
		b.WriteString(s + ";\n")
	}
	return b.String()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
