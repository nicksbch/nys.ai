package main

// Row counts (SPEC-stats.md): every file's records are counted by Go
// (encoding/csv, or line by line for NDJSON), without DuckDB's parser or
// sniffer, before it is loaded, and saved on its load_history row as
// file_stats. A CSV fails before the load if its fields per record aren't
// the columns DuckDB reads. After the insert, inside the load's transaction,
// a file whose rows in the table aren't its records fails, and its rows are
// deleted again before the commit.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// csvOpts is how Go reads a pipe's CSV files, from the pipe's options.
type csvOpts struct {
	delim     rune
	header    bool
	headerSet bool // header is in the options
}

// CSV options that don't change which lines are rows or how a row splits
// into fields, so a file read with them can still be counted by Go.
var csvKeep = map[string]bool{
	"all_varchar": true, "names": true, "column_names": true, "normalize_names": true,
	"types": true, "column_types": true, "dtypes": true, "columns": true, "auto_type_candidates": true,
	"dateformat": true, "date_format": true, "timestampformat": true, "timestamp_format": true,
	"decimal_separator": true, "nullstr": true, "allow_quoted_nulls": true, "force_not_null": true,
	"sample_size": true, "auto_detect": true, "max_line_size": true, "buffer_size": true, "parallel": true,
}

// checkStatsOptions reads delim, quote and header from the pipe's options
// and rejects the options Go can't count a file with.
func (p *pipe) checkStatsOptions() error {
	opts, err := parseOptions(p.Options)
	if err != nil {
		return err
	}
	format := p.Format
	if format == "" {
		format = p.formatOf(p.Pattern)
	}
	switch format {
	case "parquet": // counted from its footer; options don't change that
		return nil
	case "json":
		if len(opts) > 0 {
			return fmt.Errorf("stats: JSON options (%s) can change what a row is, so files can't be counted line by line; remove them or set stats: false", p.Options)
		}
		return nil
	}
	p.csv = csvOpts{delim: ','}
	for k, v := range opts {
		switch k {
		case "delim", "sep":
			r := []rune(strings.ReplaceAll(v, `\t`, "\t"))
			if len(r) != 1 || r[0] == '"' || r[0] == '\n' || r[0] == '\r' {
				return fmt.Errorf("stats: %s %q: only a one-character delimiter can be counted; set stats: false", k, v)
			}
			p.csv.delim = r[0]
		case "quote", "escape":
			if v != `"` {
				return fmt.Errorf(`stats: %s %q: files are counted with Go's encoding/csv, which only knows '"'; set stats: false`, k, v)
			}
		case "header":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("header %q: want true or false", v)
			}
			p.csv.header, p.csv.headerSet = b, true
		default:
			if !csvKeep[k] {
				return fmt.Errorf("stats: option %s may change which lines are rows, so files can't be counted without DuckDB; remove it or set stats: false", k)
			}
		}
	}
	if format == "csv" && !p.csv.headerSet {
		return fmt.Errorf("stats: set header = true or false in options, so files can be counted without DuckDB guessing it")
	}
	return nil
}

// parseOptions splits reader options ("delim = ';', names = ['a', 'b']")
// into lowercase keys and their values, string values unquoted.
func parseOptions(s string) (map[string]string, error) {
	opts := map[string]string{}
	var parts []string
	depth, quoted, start := 0, false, 0
	for i, c := range s {
		switch {
		case c == '\'':
			quoted = !quoted // '' inside a string toggles twice
		case quoted:
		case c == '[' || c == '{' || c == '(':
			depth++
		case c == ']' || c == '}' || c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("options: %q isn't key = value", strings.TrimSpace(part))
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		}
		opts[k] = v
	}
	return opts, nil
}

// fileStats is a file's count, saved as load_history.file_stats: its
// records (and a CSV's fields per record) counted by Go, without DuckDB's
// parser, and a CSV's columns as DuckDB reads it. The file fails if they
// differ, or if the rows that land in the table aren't its records.
type fileStats struct {
	Bytes   int64  `json:"bytes"`
	Skipped string `json:"skipped,omitempty"` // why it isn't counted, e.g. "zst"
	Records *int64 `json:"records,omitempty"` // counted by Go (Parquet: from the footer)
	Fields  *int64 `json:"fields,omitempty"`  // per record, counted by Go (CSV only)
	Columns *int64 `json:"columns,omitempty"` // as DuckDB reads the file (CSV only)

	how     string // how Records was counted, for messages
	problem string // why the file fails before it is loaded
}

func (fs *fileStats) json() string {
	if fs == nil {
		return ""
	}
	b, _ := json.Marshal(fs)
	return string(b)
}

// readErr is an error reading the file from S3, not in the file: the next
// poll tries again.
type readErr struct{ err error }

func (e *readErr) Error() string { return e.err.Error() }

// errReader remembers the first error of the reader it wraps that isn't EOF.
type errReader struct {
	r   io.Reader
	err error
}

func (r *errReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

// fileStats counts every file in batch, keyed by key, or returns nil if the
// pipe has stats off. A file Go can't read, or whose fields differ from
// DuckDB's columns, gets a problem. An error is DuckDB failing on a file (as
// a load would) or S3 failing (a readErr's cause, which is transient).
func (e *engine) fileStats(ctx context.Context, p *pipe, batch []object) (map[string]*fileStats, error) {
	if !p.stats() {
		return nil, nil
	}
	out := map[string]*fileStats{}
	var stmts []string
	var read []object // the files DuckDB reads next: CSV columns, Parquet rows
	for _, o := range batch {
		fs := &fileStats{Bytes: o.Size}
		out[o.Key] = fs
		switch format := p.formatOf(o.Key); {
		case strings.HasSuffix(strings.ToLower(o.Key), ".zst"):
			fs.Skipped = "zst"
		case format == "parquet":
			fs.how = "Parquet footer"
			read = append(read, o)
			stmts = append(stmts, "SELECT sum(num_rows)::BIGINT AS n FROM parquet_file_metadata("+ql(e.url(p, o))+")")
		case format == "csv" && !p.csv.headerSet:
			fs.problem = "set header = true or false in the pipe's options, so the file can be counted"
		default:
			records, fields, err := e.count(ctx, p, o, format)
			var re *readErr
			if errors.As(err, &re) {
				return nil, re.err
			}
			if err != nil {
				fs.problem = format + ": " + err.Error()
				continue
			}
			fs.Records, fs.how = &records, "non-empty lines"
			if format == "csv" {
				fs.Fields, fs.how = &fields, "encoding/csv"
				read = append(read, o)
				stmts = append(stmts, "DESCRIBE SELECT * FROM "+e.reader(p, o))
			}
		}
	}
	if len(stmts) == 0 {
		return out, nil
	}
	res, err := e.duck.run(ctx, stmts...)
	if err != nil {
		return nil, err
	}
	for i, o := range read {
		fs := out[o.Key]
		if fs.Fields == nil { // Parquet
			n := res[i][0].int("n")
			fs.Records = &n
			continue
		}
		n := int64(len(res[i]))
		fs.Columns = &n
		if *fs.Fields != n {
			fs.problem = fmt.Sprintf("columns: %d fields per record in the file (%s), %d columns read by DuckDB", *fs.Fields, fs.how, n)
		}
	}
	return out, nil
}

// reader is the table function reading o with the pipe's reader and options.
func (e *engine) reader(p *pipe, o object) string {
	args := ql(e.url(p, o))
	if p.Options != "" {
		args += ", " + p.Options
	}
	return reader[p.formatOf(o.Key)] + "(" + args + ")"
}

// count reads o from S3 and counts its records, and for a CSV the fields
// per record, with Go. A *readErr is S3 failing.
func (e *engine) count(ctx context.Context, p *pipe, o object, format string) (records, fields int64, err error) {
	var file io.ReadCloser
	if e.root != "" {
		file, err = os.Open(e.url(p, o))
	} else {
		var obj *s3.GetObjectOutput
		if obj, err = e.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(p.Bucket), Key: aws.String(o.Key)}); err == nil {
			file = obj.Body
		}
	}
	if err != nil {
		return 0, 0, &readErr{err}
	}
	defer file.Close()
	body := &errReader{r: file}
	records, fields, err = countReader(body, strings.HasSuffix(strings.ToLower(o.Key), ".gz"), format, p.csv)
	if body.err != nil {
		return 0, 0, &readErr{body.err}
	}
	return records, fields, err
}

// countReader counts the records of a file, gunzipping it first if gz.
func countReader(r io.Reader, gz bool, format string, opts csvOpts) (records, fields int64, err error) {
	br := bufio.NewReaderSize(r, 1<<20)
	r = br
	if gz {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return 0, 0, err
		}
		r = bufio.NewReaderSize(zr, 1<<20)
	}
	if format == "json" {
		n, err := countLines(r)
		return n, 0, err
	}
	return countCSV(r, opts)
}

// countCSV counts a CSV's records (less the header) and the fields of the
// first one. Every record must have that many fields. A quoted newline
// stays inside its field.
func countCSV(r io.Reader, opts csvOpts) (records, fields int64, err error) {
	br := bufio.NewReader(r)
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\xef\xbb\xbf")) { // a UTF-8 BOM, which DuckDB skips too
		br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.Comma = opts.delim
	cr.ReuseRecord = true
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
		if records == 0 {
			fields = int64(len(rec))
		}
		records++
	}
	if opts.header && records > 0 {
		records--
	}
	return records, fields, nil
}

// countLines counts the lines that aren't blank: an NDJSON file's rows.
func countLines(r io.Reader) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var n int64
	blank := true
	for {
		b, err := br.ReadSlice('\n')
		if len(bytes.TrimSpace(b)) > 0 {
			blank = false
		}
		if err == bufio.ErrBufferFull {
			continue // a long line; it goes on
		}
		if err == nil || err == io.EOF {
			if !blank {
				n++
			}
			blank = true
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// lakeCheck is the SQL that counts the rows of a file just inserted into
// target (its rows with _file = url and _loaded_at = ts). Its row has url,
// n, and mismatch: how n differs from the file's records, or NULL.
func lakeCheck(target, url, ts string, fs *fileStats) string {
	mismatch := "NULL::VARCHAR"
	if fs != nil && fs.Records != nil {
		mismatch = fmt.Sprintf("CASE WHEN count(*) <> %d THEN %s || count(*) END",
			*fs.Records, ql(fmt.Sprintf("rows: %d in the file (%s), in the table ", *fs.Records, fs.how)))
	}
	return "SELECT " + ql(url) + " AS url, count(*) AS n, " + mismatch + " AS mismatch FROM " + target +
		" WHERE _loaded_at = " + ts + " AND _file = " + ql(url)
}
