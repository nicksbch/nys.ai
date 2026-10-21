// Stream a file from a URL straight into an S3 bucket (e.g. the Garage set up by
// garage-setup.sh) without writing it to local disk. Large files are uploaded in
// parts, so memory use stays at a few chunks regardless of file size.
//
// Credentials and endpoint come from the usual AWS env vars or ~/.aws profile:
//
//	AWS_ACCESS_KEY_ID=
//	AWS_SECRET_ACCESS_KEY=
//	AWS_DEFAULT_REGION=garage
//	AWS_ENDPOINT_URL=http://192.168.1.169:3900
//
// Each run is published to relay (../relay) as "url-to-bucket.download"
// events, "started" and then "ok" or "failed", with the same run ID, when
// RELAY_URL is set or /etc/relay-client.env exists. vigil (../vigil) reads
// them from relay.
//
//	RELAY_URL=http://127.0.0.1:8091
//	RELAY_TOKEN=
//
// Usage:
//
//	go run . <url> <bucket> [key]
//	go build -o url-to-bucket . && ./url-to-bucket <url> <bucket> [key]
//	url-to-bucket serve [-workers 2]   # take requests from relay (serve.go)
//
// key defaults to the last path segment of the URL. A key ending in "/" is
// treated as a prefix and the URL's filename is appended.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	relay "relay/client"
)

const chunk = 16 * 1024 * 1024 // multipart part size

const topic = "url-to-bucket.download" // relay topic for requests and results

func defaultKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "download"
	}
	// u.Path is already unescaped.
	name := path.Base(u.Path)
	if name == "/" || name == "." || name == "" {
		return "download"
	}
	return name
}

// progress wraps a reader, counts bytes read and, if out is set, reports
// them with throughput and ETA. total and done may be read while it runs.
type progress struct {
	r     io.Reader
	out   io.Writer    // nil: count only
	total atomic.Int64 // 0 if unknown
	done  atomic.Int64

	start    time.Time
	last     time.Time // last rate update
	lastDone int64     // done at last rate update
	rate     float64   // smoothed bytes/s

	// rate and the time of its update, for readers on other goroutines
	rateBits, updated atomic.Int64
}

// speed returns the smoothed rate in bytes/s, or 0 if nothing arrived for a
// few seconds.
func (p *progress) speed() float64 {
	if time.Since(time.Unix(0, p.updated.Load())) > rateWindow {
		return 0
	}
	return math.Float64frombits(uint64(p.rateBits.Load()))
}

// Time constant for the moving average: the displayed rate mostly reflects the
// last few seconds, so it reacts to changes without jumping around.
const rateWindow = 3 * time.Second

func (p *progress) Read(b []byte) (int, error) {
	now := time.Now()
	if p.start.IsZero() {
		p.start, p.last = now, now
	}
	n, err := p.r.Read(b)
	done, total := p.done.Add(int64(n)), p.total.Load()
	// Update the rate and redraw at most 10x/s, plus once at the end.
	dt := now.Sub(p.last)
	if err == nil && dt < 100*time.Millisecond {
		return n, err
	}
	if dt > 0 {
		inst := float64(done-p.lastDone) / dt.Seconds()
		if p.rate == 0 {
			p.rate = inst
		} else {
			a := 1 - math.Exp(-dt.Seconds()/rateWindow.Seconds())
			p.rate += a * (inst - p.rate)
		}
	}
	p.last, p.lastDone = now, done
	p.rateBits.Store(int64(math.Float64bits(p.rate)))
	p.updated.Store(now.UnixNano())
	if p.out == nil {
		return n, err
	}

	line := fmt.Sprintf("%.1f MB", float64(done)/1e6)
	if total > 0 {
		pct := 100 * float64(done) / float64(total)
		line = fmt.Sprintf("%.1f / %.1f MB (%.0f%%)", float64(done)/1e6, float64(total)/1e6, pct)
	}
	line += "  " + fmtRate(p.rate)
	if total > 0 && p.rate > 0 && done < total {
		line += "  ETA " + fmtDur(time.Duration(float64(total-done)/p.rate*float64(time.Second)))
	}
	// \x1b[K clears leftovers from a longer previous line.
	fmt.Fprintf(p.out, "\r  %s\x1b[K", line)
	return n, err
}

// summary returns the average rate and elapsed time of the whole transfer.
func (p *progress) summary() string {
	el := time.Since(p.start)
	if p.start.IsZero() || el <= 0 {
		return ""
	}
	done := p.done.Load()
	return fmt.Sprintf("%.1f MB in %s, avg %s", float64(done)/1e6, fmtDur(el), fmtRate(float64(done)/el.Seconds()))
}

func fmtRate(bps float64) string {
	return fmt.Sprintf("%.1f MB/s (%.0f Mbit/s)", bps/1e6, bps*8/1e6)
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serveMain(os.Args[2:])
		return
	}
	fs := flag.NewFlagSet("url-to-bucket", flag.ExitOnError)
	endpoint := fs.String("endpoint-url", os.Getenv("AWS_ENDPOINT_URL"), "S3 endpoint (default: $AWS_ENDPOINT_URL)")
	profile := fs.String("profile", "", "AWS profile name from ~/.aws")
	contentType := fs.String("content-type", "", "override the Content-Type sent by the server")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Stream a URL into an S3 bucket.\n\nusage: %s [flags] <url> <bucket> [key]\n\n", fs.Name())
		fs.PrintDefaults()
	}

	// Allow flags anywhere on the command line, not just before positionals.
	var pos []string
	for args := os.Args[1:]; ; {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) < 2 || len(pos) > 3 {
		fs.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, pos, *endpoint, *profile, *contentType); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

// newRunID returns the random ID shared by a run's events.
func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func run(ctx context.Context, pos []string, endpoint, profile, contentType string) error {
	j := job{URL: pos[0], Bucket: pos[1], ContentType: contentType, RunID: newRunID()}
	if len(pos) == 3 {
		j.Key = pos[2]
	}
	j.Key = resolveKey(j.URL, j.Key)
	client, err := newS3Client(ctx, endpoint, profile)
	if err != nil {
		return err
	}
	return runJob(ctx, relay.FromEnv(), client, j, &progress{out: os.Stderr})
}

// resolveKey returns the object key for rawURL: key itself, the URL's
// filename if key is empty, or key + the filename if key ends in "/".
func resolveKey(rawURL, key string) string {
	if key == "" || strings.HasSuffix(key, "/") {
		return key + defaultKey(rawURL)
	}
	return key
}

func newS3Client(ctx context.Context, endpoint, profile string) (*s3.Client, error) {
	opts := []func(*config.LoadOptions) error{config.WithRetryMaxAttempts(5)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
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

// job is one download. Key is already resolved. Cause is the ID of the relay
// request that asked for it, or 0 for a run from the command line.
type job struct {
	URL, Bucket, Key, ContentType string
	RunID                         string
	Cause                         uint64
}

// runJob downloads j and publishes it to relay as "started" and then "ok" or
// "failed". prog counts the bytes, and draws them on prog.out if set.
func runJob(ctx context.Context, bus *relay.Client, client *s3.Client, j job, prog *progress) (err error) {
	// Each publish waits (up to 3s) so "started" is stored before the result
	// and nothing is lost when the program exits.
	start := time.Now()
	publish := func(typ string, data map[string]any) {
		data["url"], data["bucket"], data["key"] = j.URL, j.Bucket, j.Key
		data["name"] = j.URL + " -> s3://" + j.Bucket + "/" + j.Key
		ev := relay.Event{Topic: topic, Type: typ, RunID: j.RunID, Cause: j.Cause, Data: data}
		if j.Cause != 0 {
			// A request redelivered after a restart doesn't publish twice.
			ev.Key = fmt.Sprintf("req-%d-%s", j.Cause, typ)
		}
		rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, rerr := bus.Publish(rctx, ev); rerr != nil {
			fmt.Fprintln(os.Stderr, "warning: could not publish to relay:", rerr)
		}
	}
	publish("started", map[string]any{})
	out := prog.out
	defer func() {
		if err != nil && j.Cause != 0 && ctx.Err() != nil {
			// The service is stopping: the request is not acked and runs
			// again after the restart, so this run has no result.
			return
		}
		data := map[string]any{"duration_ms": time.Since(start).Milliseconds(), "bytes": prog.done.Load()}
		typ := "ok"
		if err != nil {
			typ, data["error"] = "failed", err.Error()
		}
		publish(typ, data)
	}()

	httpClient := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 60 * time.Second}).DialContext,
		TLSHandshakeTimeout:   60 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "url-to-bucket/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s: %s", j.URL, resp.Status)
	}

	contentType := j.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
		if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil {
			contentType = mt
		}
	}
	prog.r = resp.Body
	total, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	prog.total.Store(total)
	if out != nil {
		fmt.Fprintf(out, "%s\n  -> s3://%s/%s (%s)\n", j.URL, j.Bucket, j.Key, contentType)
	}

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = chunk
	})
	_, err = uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(j.Bucket),
		Key:         aws.String(j.Key),
		Body:        prog,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ctx.Err()
		}
		return err
	}

	if out != nil {
		fmt.Fprintf(out, "\nDone: s3://%s/%s (%s)\n", j.Bucket, j.Key, prog.summary())
	}
	return nil
}
