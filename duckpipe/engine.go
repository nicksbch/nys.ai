package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	relay "relay/client"
)

// The load history lives in the lake as <lake>.duckpipe.load_history.
const historySchema = "duckpipe"

type engine struct {
	cfg      *config
	s3       *s3.Client
	duck     *duck
	lake     string // quoted lake name
	hist     string // quoted history table
	dataPath string // the lake's own files, never loaded as input

	bus  *relay.Client   // nil (no-op) when relay isn't configured
	down map[string]bool // what last failed to poll, so relay hears of it once

	// What "run" is doing, for the page (ui.go).
	mu          sync.Mutex
	stopped     bool                    // "Stop all" on the page; "Resume" clears it
	cancel      context.CancelCauseFunc // cancels the poll or maintenance under way
	loading     *loading                // the batch being loaded, if any
	maintaining time.Time               // when maintenance started, if it is running
	pipes       map[string]pipeState    // each pipe's last poll
	poke        chan struct{}           // starts the next poll now
}

// loading is the batch being loaded, as the page shows it.
type loading struct {
	Pipe    string    `json:"pipe"`
	Table   string    `json:"table"`
	Bucket  string    `json:"bucket"`
	Keys    []string  `json:"keys"`
	Bytes   int64     `json:"bytes"`
	Started time.Time `json:"started"`
}

// pipeState is a pipe's last poll: when it ended, why it failed, and the
// files it found that are still to be loaded.
type pipeState struct {
	PolledAt     time.Time `json:"polled_at"`
	Error        string    `json:"error,omitempty"`
	Waiting      int       `json:"waiting"`
	WaitingBytes int64     `json:"waiting_bytes"`
}

// errStopped cancels the load or maintenance under way when "Stop all" is
// pressed on the page.
var errStopped = errors.New("stopped from the page")

type object struct {
	Key, ETag string
	Size      int64
	Modified  time.Time
}

// qi quotes an SQL identifier, ql an SQL string literal.
func qi(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func ql(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

func tsLit(t time.Time) string {
	return "TIMESTAMPTZ " + ql(t.UTC().Format("2006-01-02 15:04:05.000000+00"))
}

func open(ctx context.Context, cfg *config, duckdbBin, endpoint, profile, region, stateDir string) (*engine, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRetryMaxAttempts(5)}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	if awsCfg.Region == "" {
		awsCfg.Region = "garage"
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		// Garage serves buckets by path, not as <bucket>.<host> subdomains.
		o.UsePathStyle = true
		// Only send checksums S3 requires; avoids issues with non-AWS stores.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	creds, err := awsCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS credentials: %w", err)
	}

	e := &engine{
		bus:   relay.FromEnv(),
		down:  map[string]bool{},
		pipes: map[string]pipeState{},
		poke:  make(chan struct{}, 1),
		cfg:   cfg,
		s3:    client,
		lake:  qi(cfg.Lake.Name),
		hist:  qi(cfg.Lake.Name) + "." + qi(historySchema) + ".load_history",
	}

	// Partitions by day of _loaded_at are cut in the session time zone, so pin
	// it: a day is a UTC day, whatever the server's TZ.
	setup := []string{"SET TimeZone = 'UTC'"}
	if stateDir != "" {
		setup = append(setup,
			"SET extension_directory = "+ql(filepath.Join(stateDir, "extensions")),
			"SET temp_directory = "+ql(filepath.Join(stateDir, "tmp")),
		)
	}
	// DuckDB reads the files itself, so it gets the same S3 settings as the
	// lister. The script goes to duckdb on stdin, so the secret never shows up
	// in a process list, and the secret is temporary: nothing is saved.
	secret := []string{"TYPE s3", "REGION " + ql(awsCfg.Region), "URL_STYLE 'path'"}
	if creds.AccessKeyID != "" {
		secret = append(secret, "KEY_ID "+ql(creds.AccessKeyID), "SECRET "+ql(creds.SecretAccessKey))
		if creds.SessionToken != "" {
			secret = append(secret, "SESSION_TOKEN "+ql(creds.SessionToken))
		}
	}
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("endpoint %q: want a URL like http://host:3900", endpoint)
		}
		secret = append(secret, "ENDPOINT "+ql(u.Host), fmt.Sprintf("USE_SSL %t", u.Scheme == "https"))
	}
	attach := "ATTACH " + ql(cfg.Lake.Catalog) + " AS " + e.lake
	if cfg.Lake.DataPath != "" {
		attach += " (DATA_PATH " + ql(cfg.Lake.DataPath) + ")"
	}
	setup = append(setup,
		"LOAD httpfs", "LOAD ducklake",
		"CREATE TEMPORARY SECRET duckpipe_s3 ("+strings.Join(secret, ", ")+")",
		attach,
	)

	bin, err := lookPath(duckdbBin)
	if err != nil {
		return nil, err
	}
	// Install the extensions and create the history table once, with a plain
	// setup, so errors here say what is missing.
	plain := &duck{bin: bin, setup: setup[:len(setup)-4]}
	res, err := plain.query(ctx, "INSTALL httpfs", "INSTALL ducklake", "SELECT version() AS v")
	if err != nil {
		return nil, fmt.Errorf("installing DuckDB extensions: %w", err)
	}
	var major, minor int
	v := res[0].str("v")
	fmt.Sscanf(v, "v%d.%d", &major, &minor)
	if major < 1 || major == 1 && minor < 5 {
		return nil, fmt.Errorf("%s is DuckDB %s; duckpipe needs 1.5 or newer", bin, v)
	}

	e.duck = &duck{bin: bin, setup: setup}
	res, err = e.duck.query(ctx,
		"CREATE SCHEMA IF NOT EXISTS "+e.lake+"."+qi(historySchema),
		`CREATE TABLE IF NOT EXISTS `+e.hist+` (
			pipe VARCHAR, bucket VARCHAR, key VARCHAR, etag VARCHAR, size BIGINT,
			last_modified TIMESTAMPTZ, status VARCHAR, rows BIGINT, error VARCHAR,
			loaded_at TIMESTAMPTZ, target VARCHAR)`,
		// Histories from before target was recorded.
		"ALTER TABLE "+e.hist+" ADD COLUMN IF NOT EXISTS target VARCHAR",
		"SELECT value AS data_path FROM ducklake_options("+ql(cfg.Lake.Name)+") WHERE option_name = 'data_path'",
	)
	if err != nil {
		return nil, fmt.Errorf("opening the lake: %w", err)
	}
	// Never feed the lake's own Parquet files back in as input.
	e.dataPath = cfg.Lake.DataPath
	if len(res) == 1 && res[0].str("data_path") != "" {
		e.dataPath = res[0].str("data_path")
	}
	return e, nil
}

func lookPath(bin string) (string, error) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("duckdb CLI %q not found (install DuckDB 1.5+ or pass -duckdb /path/to/duckdb): %w", bin, err)
	}
	return p, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// cycle polls every pipe once. It reports whether any pipe could not be
// polled; errors are logged.
func (e *engine) cycle(ctx context.Context) (failed bool) {
	done, err := e.seen(ctx)
	if ctx.Err() != nil {
		return true
	}
	e.polled("load history", err)
	if err != nil {
		log.Printf("reading load history: %v (will retry next poll)", err)
		for _, p := range e.cfg.Pipes {
			e.setPolled(p.Name, fmt.Errorf("reading load history: %w", err))
		}
		return true
	}
	for _, p := range e.cfg.Pipes {
		if ctx.Err() != nil {
			return true
		}
		err := e.poll(ctx, p, done[p.Name+"\x00"+p.Bucket])
		if ctx.Err() != nil {
			return true
		}
		e.polled(p.Name, err)
		e.setPolled(p.Name, err)
		if err != nil {
			failed = true
			log.Printf("%s: %v (will retry next poll)", p.Name, err)
		}
	}
	return failed
}

// polled tells relay when a pipe (or the load history) starts failing to be
// polled, and when it recovers, rather than on every poll.
func (e *engine) polled(name string, err error) {
	if (err != nil) == e.down[name] {
		return
	}
	e.down[name] = err != nil
	e.publish("poll", result(err), "", map[string]any{"name": name}, err)
}

// result is the event type for the outcome of a job: "ok" or "failed".
func result(err error) string {
	if err != nil {
		return "failed"
	}
	return "ok"
}

// publish sends a "duckpipe.<kind>" event to relay, waiting at most a few
// seconds. A relay that is down only gets logged. A non-nil err goes in
// data.error.
func (e *engine) publish(kind, typ, runID string, data map[string]any, err error) {
	if err != nil {
		data["error"] = err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, perr := e.bus.Publish(ctx, relay.Event{Topic: "duckpipe." + kind, Type: typ, RunID: runID, Data: data}); perr != nil {
		log.Printf("could not publish to relay: %v", perr)
	}
}

// newRunID returns the random ID shared by a load's events.
func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// poll lists the pipe's prefix and loads every object not loaded (or failed)
// before, oldest first, in batches.
func (e *engine) poll(ctx context.Context, p *pipe, done map[string]bool) error {
	var todo []object
	pg := s3.NewListObjectsV2Paginator(e.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(p.Bucket),
		Prefix: aws.String(p.Prefix),
	})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing s3://%s/%s: %w", p.Bucket, p.Prefix, err)
		}
		for _, o := range page.Contents {
			obj := object{
				Key:      aws.ToString(o.Key),
				ETag:     strings.Trim(aws.ToString(o.ETag), `"`),
				Size:     aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified),
			}
			if obj.Size == 0 || !p.matches(obj.Key) || done[obj.Key+"\x00"+obj.ETag] {
				continue
			}
			if e.dataPath != "" && strings.HasPrefix(e.url(p, obj), e.dataPath) {
				continue
			}
			todo = append(todo, obj)
		}
	}
	e.setWaiting(p.Name, todo)
	if len(todo) == 0 {
		return nil
	}
	sort.Slice(todo, func(i, j int) bool {
		if !todo[i].Modified.Equal(todo[j].Modified) {
			return todo[i].Modified.Before(todo[j].Modified)
		}
		return todo[i].Key < todo[j].Key
	})

	// Files that can't be read at all are recorded as failed straight away.
	ok := todo[:0]
	for _, o := range todo {
		var why string
		switch {
		case p.formatOf(o.Key) == "":
			why = "unknown file type; set format in the pipe"
		case strings.ContainsAny(o.Key, "*?[{"):
			why = "key contains glob characters (* ? [ {), which DuckDB would expand"
		}
		if why == "" {
			ok = append(ok, o)
			continue
		}
		e.publish("ingest", "failed", "", e.ingestData(p, []object{o}), errors.New(why))
		if err := e.fail(ctx, p, o, errors.New(why)); err != nil {
			return err
		}
	}
	todo = ok

	for len(todo) > 0 {
		n := min(p.Batch, len(todo))
		batch := todo[:n]
		todo = todo[n:]
		if err := e.loadBatch(ctx, p, batch); err != nil {
			return err
		}
		e.setWaiting(p.Name, todo)
	}
	return nil
}

// loadBatch loads files in one transaction. If that fails because of a file,
// it loads them one at a time, so only the bad files are marked failed.
// Each attempt is published to relay as "duckpipe.ingest": "started", then
// "ok" or "failed" with the same run ID.
func (e *engine) loadBatch(ctx context.Context, p *pipe, batch []object) error {
	start := time.Now()
	runID := newRunID()
	e.publish("ingest", "started", runID, e.ingestData(p, batch), nil)
	e.setLoading(p, batch, start)
	rows, err := e.load(ctx, p, batch)
	e.setLoading(nil, nil, time.Time{})
	if ctx.Err() != nil {
		err = context.Cause(ctx) // e.g. errStopped
	}
	data := e.ingestData(p, batch)
	data["duration_ms"] = time.Since(start).Milliseconds()
	if err == nil {
		data["items"] = rows
	}
	e.publish("ingest", result(err), runID, data, err)
	if err == nil {
		log.Printf("%s: loaded %d file(s), %d rows into %s in %s",
			p.Name, len(batch), rows, e.targetName(p), time.Since(start).Round(time.Millisecond))
		return nil
	}
	if transient(ctx, err) {
		return err
	}
	if len(batch) == 1 {
		log.Printf("%s: s3://%s/%s failed: %v", p.Name, p.Bucket, batch[0].Key, firstLine(err.Error()))
		return e.fail(ctx, p, batch[0], err)
	}
	for _, o := range batch {
		if err := e.loadBatch(ctx, p, []object{o}); err != nil {
			return err
		}
	}
	return nil
}

// ingestData describes a batch for relay: name ("pipe: file -> table"),
// pipe, table, files and bytes. A batch of several files is named by its
// first and last file, so batches from the same prefix can be told apart.
func (e *engine) ingestData(p *pipe, batch []object) map[string]any {
	name := e.url(p, batch[0])
	if len(batch) > 1 {
		first := strings.TrimPrefix(batch[0].Key, p.Prefix)
		last := strings.TrimPrefix(batch[len(batch)-1].Key, p.Prefix)
		name = fmt.Sprintf("%d files from s3://%s/%s (%s … %s)", len(batch), p.Bucket, p.Prefix, first, last)
	}
	var size int64
	for _, o := range batch {
		size += o.Size
	}
	return map[string]any{"name": p.Name + ": " + name + " -> " + e.targetName(p),
		"pipe": p.Name, "table": e.targetName(p), "files": len(batch), "bytes": size}
}

func (e *engine) url(p *pipe, o object) string {
	return "s3://" + p.Bucket + "/" + o.Key
}

func (e *engine) target(p *pipe) string {
	return e.lake + "." + qi(p.schema) + "." + qi(p.table)
}

// targetName is the pipe's table as shown in status and the load history.
func (e *engine) targetName(p *pipe) string {
	return e.cfg.Lake.Name + "." + p.schema + "." + p.table
}

// load inserts the files into the pipe's table and records them in the load
// history, all in one transaction. It returns the number of rows loaded.
func (e *engine) load(ctx context.Context, p *pipe, batch []object) (int64, error) {
	// Every row gets the file it came from and the load time. The timestamp is
	// unique to this load, which also lets us count rows per file.
	ts := tsLit(time.Now())
	parts := make([]string, len(batch))
	files := make([]string, len(batch))
	for i, o := range batch {
		src := ql(e.url(p, o))
		args := src
		if p.Options != "" {
			args += ", " + p.Options
		}
		parts[i] = fmt.Sprintf("SELECT *, %s AS _file, %s AS _loaded_at FROM %s(%s)", src, ts, reader[p.formatOf(o.Key)], args)
		files[i] = fmt.Sprintf("(%s, %s, %d, %s, %s)", ql(o.Key), ql(o.ETag), o.Size, tsLit(o.Modified), src)
	}
	src := strings.Join(parts, "\nUNION ALL BY NAME\n")

	ddl, err := e.tableDDL(ctx, p, src)
	if err != nil {
		return 0, err
	}
	stmts := append([]string{"BEGIN"}, ddl...)
	stmts = append(stmts,
		"INSERT INTO "+e.target(p)+" BY NAME "+src,
		`INSERT INTO `+e.hist+`
		SELECT `+ql(p.Name)+`, `+ql(p.Bucket)+`, f.key, f.etag, f.size, f.modified,
			'loaded', coalesce(c.n, 0), NULL, now(), `+ql(e.targetName(p))+`
		FROM (VALUES `+strings.Join(files, ", ")+`) f(key, etag, size, modified, url)
		LEFT JOIN (SELECT _file, count(*) AS n FROM `+e.target(p)+` WHERE _loaded_at = `+ts+` GROUP BY ALL) c
			ON c._file = f.url`,
		"COMMIT",
		"SELECT count(*) AS n FROM "+e.target(p)+" WHERE _loaded_at = "+ts,
	)
	res, err := e.duck.query(ctx, stmts...)
	if err != nil {
		return 0, err
	}
	for _, s := range ddl {
		log.Printf("%s: %s", p.Name, firstLine(s))
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0].int("n"), nil
}

// partitionBy splits a table's Parquet files by the UTC day they were loaded.
const partitionBy = "SET PARTITIONED BY (year(_loaded_at), month(_loaded_at), day(_loaded_at))"

// tableDDL returns the statements that create the pipe's table from the
// files' columns, or add the columns it lacks. Existing columns are never
// changed: a value that doesn't fit its column's type fails the file.
// A table without a partition spec (new, or from before duckpipe set one)
// gets partitionBy; one someone partitioned otherwise is left alone.
func (e *engine) tableDDL(ctx context.Context, p *pipe, src string) ([]string, error) {
	meta := qi("__ducklake_metadata_" + e.cfg.Lake.Name)
	res, err := e.duck.run(ctx,
		"SELECT column_name, column_type FROM (DESCRIBE "+src+")",
		"SELECT column_name FROM duckdb_columns() WHERE database_name = "+ql(e.cfg.Lake.Name)+
			" AND schema_name = "+ql(p.schema)+" AND table_name = "+ql(p.table),
		`SELECT count(*) AS n FROM `+meta+`.ducklake_partition_info pi
		JOIN `+meta+`.ducklake_table t ON t.table_id = pi.table_id AND t.end_snapshot IS NULL
		JOIN `+meta+`.ducklake_schema s ON s.schema_id = t.schema_id AND s.end_snapshot IS NULL
		WHERE pi.end_snapshot IS NULL AND s.schema_name = `+ql(p.schema)+` AND t.table_name = `+ql(p.table),
	)
	if err != nil {
		return nil, err
	}
	alter := "ALTER TABLE " + e.target(p) + " "
	have := map[string]bool{}
	for _, r := range res[1] {
		have[strings.ToLower(r.str("column_name"))] = true
	}

	if len(have) == 0 {
		defs := make([]string, len(res[0]))
		for i, c := range res[0] {
			defs[i] = qi(c.str("column_name")) + " " + c.str("column_type")
		}
		return []string{
			"CREATE SCHEMA IF NOT EXISTS " + e.lake + "." + qi(p.schema),
			"CREATE TABLE " + e.target(p) + " (" + strings.Join(defs, ", ") + ")",
			alter + partitionBy,
		}, nil
	}
	var ddl []string
	for _, c := range res[0] {
		if !have[strings.ToLower(c.str("column_name"))] {
			ddl = append(ddl, alter+"ADD COLUMN "+qi(c.str("column_name"))+" "+c.str("column_type"))
		}
	}
	if len(res[2]) == 0 || res[2][0].int("n") == 0 {
		ddl = append(ddl, alter+partitionBy)
	}
	return ddl, nil
}

// fail records a file as failed, so it is skipped until it changes or someone
// runs "duckpipe retry".
func (e *engine) fail(ctx context.Context, p *pipe, o object, cause error) error {
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	_, err := e.duck.run(ctx, fmt.Sprintf("INSERT INTO %s VALUES (%s, %s, %s, %s, %d, %s, 'failed', 0, %s, now(), %s)",
		e.hist, ql(p.Name), ql(p.Bucket), ql(o.Key), ql(o.ETag), o.Size, tsLit(o.Modified), ql(msg), ql(e.targetName(p))))
	return err
}

// seen returns, per pipe and bucket, the key+ETag pairs already loaded or
// failed. It is read fresh on every poll, so "duckpipe retry" takes effect
// without a restart.
func (e *engine) seen(ctx context.Context) (map[string]map[string]bool, error) {
	rows, err := e.duck.query(ctx, "SELECT DISTINCT pipe, bucket, key, etag FROM "+e.hist)
	if err != nil {
		return nil, err
	}
	done := map[string]map[string]bool{}
	for _, r := range rows {
		pb := r.str("pipe") + "\x00" + r.str("bucket")
		if done[pb] == nil {
			done[pb] = map[string]bool{}
		}
		done[pb][r.str("key")+"\x00"+r.str("etag")] = true
	}
	return done, nil
}

// stats returns, per pipe, the files and rows loaded, the files whose latest
// attempt failed and the last load (UTC, "2006-01-02 15:04:05"), and the 50
// newest failed files.
func (e *engine) stats(ctx context.Context) (map[string]row, []row, error) {
	// A file counts as failed only if its latest attempt failed; a fixed file
	// that loaded later doesn't.
	latest := `(SELECT *, row_number() OVER (PARTITION BY pipe, bucket, key ORDER BY loaded_at DESC) = 1 AS latest
		FROM ` + e.hist + `)`
	res, err := e.duck.run(ctx,
		`SELECT pipe,
			count(*) FILTER (status = 'loaded') AS files,
			coalesce(sum(rows) FILTER (status = 'loaded'), 0)::BIGINT AS rows,
			count(*) FILTER (status = 'failed' AND latest) AS failed,
			strftime(max(loaded_at) FILTER (status = 'loaded'), '%Y-%m-%d %H:%M:%S') AS last
		FROM `+latest+` GROUP BY pipe`,
		`SELECT pipe, key, error FROM `+latest+`
		WHERE latest AND status = 'failed'
		ORDER BY loaded_at DESC LIMIT 50`,
	)
	if err != nil {
		return nil, nil, err
	}
	stats := map[string]row{}
	for _, r := range res[0] {
		stats[r.str("pipe")] = r
	}
	return stats, res[1], nil
}

func (e *engine) status(ctx context.Context, w io.Writer) error {
	stats, failed, err := e.stats(ctx)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PIPE\tSOURCE\tTABLE\tFILES\tROWS\tFAILED\tLAST LOAD")
	for _, p := range e.cfg.Pipes {
		s := stats[p.Name]
		last := s.str("last")
		if last == "" {
			last = "-"
		}
		src := "s3://" + p.Bucket + "/" + p.Prefix + p.Pattern
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", p.Name, src, e.targetName(p),
			s.int("files"), s.int("rows"), s.int("failed"), last)
	}
	tw.Flush()

	for i, r := range failed {
		if i == 0 {
			fmt.Fprintln(w, "\nFailed files (newest first; fix them or the pipe, then run \"duckpipe retry\"):")
		}
		fmt.Fprintf(w, "  %s: %s\n      %s\n", r.str("pipe"), r.str("key"), firstLine(r.str("error")))
	}
	return nil
}

// historyRows returns the load history, newest first: every file loaded or
// failed, optionally for one pipe only, at most limit rows (0: all). at is
// UTC, "2006-01-02 15:04:05".
func (e *engine) historyRows(ctx context.Context, pipe string, limit int) ([]row, error) {
	q := `SELECT strftime(loaded_at, '%Y-%m-%d %H:%M:%S') AS at, pipe, 's3://' || bucket || '/' || key AS file,
			coalesce(target, '-') AS target, status, rows, size, error
		FROM ` + e.hist
	if pipe != "" {
		q += " WHERE pipe = " + ql(pipe)
	}
	q += " ORDER BY loaded_at DESC, key"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	return e.duck.query(ctx, q)
}

// history lists every file loaded or failed, newest first, optionally for
// one pipe only.
func (e *engine) history(ctx context.Context, w io.Writer, pipe string) error {
	rows, err := e.historyRows(ctx, pipe, 0)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LOADED AT\tPIPE\tFILE\tTARGET\tSTATUS\tROWS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", r.str("at"), r.str("pipe"), r.str("file"),
			r.str("target"), r.str("status"), r.int("rows"))
	}
	return tw.Flush()
}

// retry forgets failed files, so the next poll tries them again.
func (e *engine) retry(ctx context.Context, pipe string) error {
	where := "status = 'failed'"
	if pipe != "" {
		where += " AND pipe = " + ql(pipe)
	}
	res, err := e.duck.run(ctx,
		"SELECT count(*) AS n FROM "+e.hist+" WHERE "+where,
		"DELETE FROM "+e.hist+" WHERE "+where)
	if err != nil {
		return err
	}
	fmt.Printf("%d failed file(s) will be retried on the next poll\n", res[0][0].int("n"))
	return nil
}

// maintain writes out small loads kept in the catalog, merges small Parquet
// files, and drops old snapshots if configured to.
func (e *engine) maintain(ctx context.Context) error {
	start := time.Now()
	name := ql(e.cfg.Lake.Name)
	steps := []string{
		"CALL ducklake_flush_inlined_data(" + name + ")",
		"CALL ducklake_merge_adjacent_files(" + name + ")",
	}
	if keep := e.cfg.Maintenance.KeepSnapshots.Duration; keep > 0 {
		older := fmt.Sprintf("now() - INTERVAL '%d seconds'", int64(keep.Seconds()))
		steps = append(steps,
			"CALL ducklake_expire_snapshots("+name+", older_than => "+older+")",
			"CALL ducklake_cleanup_old_files("+name+", cleanup_all => true)",
		)
	}
	if _, err := e.duck.run(ctx, steps...); err != nil {
		return err
	}
	log.Printf("maintenance done in %s", time.Since(start).Round(time.Millisecond))
	return nil
}
