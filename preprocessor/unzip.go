package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	relay "relay/client"
)

const chunk = 16 * 1024 * 1024 // multipart part size

type engine struct {
	cfg     *config
	s3      *s3.Client
	up      *manager.Uploader
	state   *store
	workDir string // archives are downloaded here while they are unzipped

	bus  *relay.Client   // nil (no-op) when relay isn't configured
	down map[string]bool // what last failed to poll, so relay hears of it once
	poke chan struct{}   // a poll asked for by the page

	// What the page shows besides state.json.
	mu     sync.Mutex
	listed map[string]listing // job name -> its archives at the last poll
	busy   *unzipping         // the archive being unzipped, if any
}

type object struct {
	Key      string    `json:"key"`
	ETag     string    `json:"etag"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// listing is a job's archives as its last poll found them.
type listing struct {
	At       time.Time // of the last successful listing
	Err      string    // of the last poll, if it failed
	Archives []object
}

type unzipping struct {
	Job     string    `json:"job"`
	Key     string    `json:"key"`
	Size    int64     `json:"size"`
	Started time.Time `json:"started"`
}

// doneIDs returns the archives (job, bucket, key, ETag) not to unzip again.
func doneIDs(rs []record) map[string]bool {
	done := map[string]bool{}
	for _, r := range rs {
		if !r.Redone {
			done[r.id()] = true
		}
	}
	return done
}

// badArchive is an error in the archive itself (corrupt, unsafe entry
// names): trying again won't help until the archive is replaced.
type badArchive struct{ err error }

func (b badArchive) Error() string { return b.err.Error() }
func (b badArchive) Unwrap() error { return b.err }

func newS3(ctx context.Context, endpoint, profile, region string) (*s3.Client, error) {
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
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		// Garage serves buckets by path, not as <bucket>.<host> subdomains.
		o.UsePathStyle = true
		// Only send checksums S3 requires; avoids issues with non-AWS stores.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}), nil
}

// cycle polls every job once. It reports whether any job could not be
// polled; errors are logged.
func (e *engine) cycle(ctx context.Context) (failed bool) {
	rs, err := e.state.load()
	e.polled("state", err)
	if err != nil {
		log.Printf("reading %s: %v (will retry next poll)", e.state.file, err)
		return true
	}
	done := doneIDs(rs)
	for _, j := range e.cfg.Jobs {
		if ctx.Err() != nil {
			return true
		}
		err := e.poll(ctx, j, done)
		if ctx.Err() != nil {
			return true
		}
		e.polled(j.Name, err)
		if err != nil {
			e.mu.Lock()
			l := e.listed[j.Name]
			l.Err = err.Error()
			e.listed[j.Name] = l
			e.mu.Unlock()
		}
		if err != nil {
			failed = true
			log.Printf("%s: %v (will retry next poll)", j.Name, err)
		}
	}
	return failed
}

// polled tells relay when a job (or the state file) starts failing to be
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

// publish sends a "preprocessor.<kind>" event to relay, waiting at most a few
// seconds. A relay that is down only gets logged. A non-nil err goes in
// data.error.
func (e *engine) publish(kind, typ, runID string, data map[string]any, err error) {
	if err != nil {
		data["error"] = err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, perr := e.bus.Publish(ctx, relay.Event{Topic: "preprocessor." + kind, Type: typ, RunID: runID, Data: data}); perr != nil {
		log.Printf("could not publish to relay: %v", perr)
	}
}

// newRunID returns the random ID shared by an archive's events.
func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// poll lists the job's prefix and unzips every archive without a record for
// its current ETag, oldest first, one at a time.
func (e *engine) poll(ctx context.Context, j *job, done map[string]bool) error {
	var all, todo []object
	pg := s3.NewListObjectsV2Paginator(e.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(j.Bucket),
		Prefix: aws.String(j.Prefix),
	})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing s3://%s/%s: %w", j.Bucket, j.Prefix, err)
		}
		for _, o := range page.Contents {
			obj := object{
				Key:      aws.ToString(o.Key),
				ETag:     strings.Trim(aws.ToString(o.ETag), `"`),
				Size:     aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified),
			}
			if obj.Size == 0 || !j.matches(obj.Key) {
				continue
			}
			all = append(all, obj)
			r := record{Job: j.Name, Bucket: j.Bucket, Key: obj.Key, ETag: obj.ETag}
			if !done[r.id()] {
				todo = append(todo, obj)
			}
		}
	}
	e.mu.Lock()
	e.listed[j.Name] = listing{At: time.Now().UTC(), Archives: all}
	e.mu.Unlock()
	sort.Slice(todo, func(a, b int) bool {
		if !todo[a].Modified.Equal(todo[b].Modified) {
			return todo[a].Modified.Before(todo[b].Modified)
		}
		return todo[a].Key < todo[b].Key
	})
	for _, o := range todo {
		if err := e.process(ctx, j, o); err != nil {
			return err
		}
	}
	return nil
}

// process unzips one archive and records the outcome. Each attempt is
// published to relay as "preprocessor.unzip": "started", then "ok" or
// "failed" with the same run ID. Failures are recorded too, so a broken
// archive (or a missing permission) isn't downloaded again on every poll;
// "preprocessor retry" tries them again. Only a stop (ctx cancelled) is
// returned, unrecorded.
func (e *engine) process(ctx context.Context, j *job, o object) error {
	start := time.Now()
	runID := newRunID()
	src := "s3://" + j.Bucket + "/" + o.Key
	data := func() map[string]any {
		return map[string]any{"name": j.Name + ": " + src + " -> s3://" + j.Bucket + "/" + j.To,
			"job": j.Name, "bucket": j.Bucket, "key": o.Key, "to": j.To, "bytes": o.Size}
	}
	e.publish("unzip", "started", runID, data(), nil)
	log.Printf("%s: unzipping %s (%s)", j.Name, src, size(o.Size))

	e.mu.Lock()
	e.busy = &unzipping{Job: j.Name, Key: o.Key, Size: o.Size, Started: start.UTC()}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.busy = nil
		e.mu.Unlock()
	}()
	r, err := e.unzip(ctx, j, o)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	d := data()
	d["duration_ms"] = time.Since(start).Milliseconds()
	d["entries"], d["written"], d["skipped"] = r.Entries, r.Written, r.Skipped
	if err == nil {
		d["items"] = r.Written
	}
	e.publish("unzip", result(err), runID, d, err)

	r.Job, r.Bucket, r.Key, r.ETag, r.Size, r.At = j.Name, j.Bucket, o.Key, o.ETag, o.Size, time.Now().UTC()
	r.Status = result(err)
	if err != nil {
		msg := err.Error()
		if len(msg) > 2000 {
			msg = msg[:2000] + "…"
		}
		r.Error = msg
		log.Printf("%s: %s failed: %v", j.Name, src, err)
	} else {
		log.Printf("%s: %s: %d entries, %d written, %d unchanged, in %s",
			j.Name, src, r.Entries, r.Written, r.Skipped, time.Since(start).Round(time.Second))
	}
	return e.state.add(r)
}

// unzip downloads the archive to the work dir and writes each entry to
// j.To + its path in the archive. An entry already there with the same CRC-32
// and size (from an earlier, overlapping archive) is skipped.
func (e *engine) unzip(ctx context.Context, j *job, o object) (record, error) {
	var r record
	f, err := os.CreateTemp(e.workDir, "preprocessor-*.zip")
	if err != nil {
		return r, err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	// IfMatch: an archive replaced while downloading fails here and is
	// picked up again, under its new ETag, on the next poll.
	obj, err := e.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket:  aws.String(j.Bucket),
		Key:     aws.String(o.Key),
		IfMatch: aws.String(`"` + o.ETag + `"`),
	})
	if err != nil {
		return r, fmt.Errorf("downloading: %w", err)
	}
	n, err := io.Copy(f, obj.Body)
	obj.Body.Close()
	if err != nil {
		return r, fmt.Errorf("downloading: %w", err)
	}

	zr, err := zip.NewReader(f, n)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) { // entry names are checked below
		return r, badArchive{err}
	}
	// Check every name before writing anything, so a bad archive writes nothing.
	var files []*zip.File
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		if _, err := j.entryKey(zf.Name); err != nil {
			return r, badArchive{err}
		}
		files = append(files, zf)
	}
	r.Entries = len(files)

	for _, zf := range files {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		key, _ := j.entryKey(zf.Name)
		crc := fmt.Sprintf("%08x", zf.CRC32)
		same, err := e.unchanged(ctx, j.Bucket, key, crc, int64(zf.UncompressedSize64))
		if err != nil {
			return r, fmt.Errorf("%s: %w", zf.Name, err)
		}
		if same {
			r.Skipped++
			continue
		}
		if err := e.put(ctx, j.Bucket, key, zf, crc, "s3://"+j.Bucket+"/"+o.Key); err != nil {
			return r, fmt.Errorf("%s: %w", zf.Name, err)
		}
		r.Written++
	}
	return r, nil
}

// unchanged reports whether key already holds an entry with this CRC-32 and
// size, written by an earlier archive.
func (e *engine) unchanged(ctx context.Context, bucket, key, crc string, size int64) (bool, error) {
	h, err := e.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return h.Metadata["crc32"] == crc && aws.ToInt64(h.ContentLength) == size, nil
}

// put uploads one entry, in parts, with its CRC-32 and the archive it came
// from as metadata.
func (e *engine) put(ctx context.Context, bucket, key string, zf *zip.File, crc, source string) error {
	rc, err := zf.Open()
	if err != nil {
		return badArchive{err}
	}
	defer rc.Close()
	body := &entryReader{r: rc}
	_, err = e.up.Upload(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		Body:     body,
		Metadata: map[string]string{"crc32": crc, "source": source},
	})
	if body.err != nil { // corrupt data or a CRC mismatch, not the upload
		return badArchive{body.err}
	}
	return err
}

// entryReader remembers the error reading an entry gave, so a corrupt
// archive can be told apart from a failed upload.
type entryReader struct {
	r   io.Reader
	err error
}

func (er *entryReader) Read(p []byte) (int, error) {
	n, err := er.r.Read(p)
	if err != nil && err != io.EOF {
		er.err = err
	}
	return n, err
}

// cleanWorkDir removes archives left behind by a run that was killed.
func (e *engine) cleanWorkDir() {
	old, _ := filepath.Glob(filepath.Join(e.workDir, "preprocessor-*.zip"))
	for _, f := range old {
		os.Remove(f)
	}
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d B", n)
}

// latest returns each archive's newest record, newest first: an archive
// whose current ETag failed counts as failed even if an older version was
// unzipped.
func latest(rs []record) []record {
	seen := map[string]bool{}
	var out []record
	for i := len(rs) - 1; i >= 0; i-- {
		k := rs[i].Job + "\x00" + rs[i].Bucket + "\x00" + rs[i].Key
		if !seen[k] {
			seen[k] = true
			out = append(out, rs[i])
		}
	}
	return out
}

func (e *engine) status(w io.Writer) error {
	rs, err := e.state.load()
	if err != nil {
		return err
	}
	type stats struct {
		ok, failed, written int
		last                time.Time
	}
	by := map[string]*stats{}
	for _, j := range e.cfg.Jobs {
		by[j.Name] = &stats{}
	}
	for _, r := range latest(rs) {
		s := by[r.Job]
		if s == nil { // a job since removed from the config
			continue
		}
		if r.Status == "ok" {
			s.ok++
			if r.At.After(s.last) {
				s.last = r.At
			}
		} else {
			s.failed++
		}
	}
	for _, r := range rs {
		if s := by[r.Job]; s != nil {
			s.written += r.Written
		}
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tSOURCE\tTO\tARCHIVES\tFILES WRITTEN\tFAILED\tLAST")
	for _, j := range e.cfg.Jobs {
		s := by[j.Name]
		last := "-"
		if !s.last.IsZero() {
			last = s.last.Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(tw, "%s\ts3://%s/%s%s\ts3://%s/%s\t%d\t%d\t%d\t%s\n",
			j.Name, j.Bucket, j.Prefix, j.Pattern, j.Bucket, j.To, s.ok, s.written, s.failed, last)
	}
	tw.Flush()

	n := 0
	for _, r := range latest(rs) {
		if r.Status != "failed" || by[r.Job] == nil {
			continue
		}
		if n == 0 {
			fmt.Fprintln(w, "\nFailed archives (newest first; fix them, then run \"preprocessor retry\"):")
		}
		fmt.Fprintf(w, "  %s: s3://%s/%s\n      %s\n", r.Job, r.Bucket, r.Key, r.Error)
		n++
	}
	return nil
}

// history lists every archive unzipped or failed, newest first, optionally
// for one job only.
func (e *engine) history(w io.Writer, job string) error {
	rs, err := e.state.load()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tJOB\tARCHIVE\tSTATUS\tENTRIES\tWRITTEN\tUNCHANGED")
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		if job != "" && r.Job != job {
			continue
		}
		status := r.Status
		if r.Redone {
			status += " (redone)"
		}
		fmt.Fprintf(tw, "%s\t%s\ts3://%s/%s\t%s\t%d\t%d\t%d\n", r.At.Format("2006-01-02 15:04:05"),
			r.Job, r.Bucket, r.Key, status, r.Entries, r.Written, r.Skipped)
	}
	return tw.Flush()
}

// retry forgets failed archives, so the next poll tries them again.
func (e *engine) retry(job string) error {
	n := 0
	err := e.state.update(func(rs []record) []record {
		keep := rs[:0]
		for _, r := range rs {
			if r.Status == "failed" && (job == "" || r.Job == job) {
				n++
				continue
			}
			keep = append(keep, r)
		}
		return keep
	})
	if err != nil {
		return err
	}
	fmt.Printf("%d failed archive(s) will be retried on the next poll\n", n)
	return nil
}
