// A small web UI for an S3 store such as the Garage set up by garage-setup.sh:
// browse buckets and folders, upload (files, folders, drag and drop), download,
// delete, rename, move and copy. Folders are handled recursively.
//
// Credentials come from the usual AWS env vars or ~/.aws profile:
//
//	AWS_ACCESS_KEY_ID=
//	AWS_SECRET_ACCESS_KEY=
//	AWS_DEFAULT_REGION=garage                  (default: garage)
//	AWS_ENDPOINT_URL=http://192.168.1.169:3900 (default)
//
// Usage:
//
//	go run .                       # then open http://127.0.0.1:8080
//	go build -o garage-ui . && ./garage-ui -listen :8080 -auth admin:secret
//
// The UI has full read/write access with the configured key. It listens on
// localhost by default; set -auth (or GARAGE_UI_AUTH=user:pass) before exposing
// it on the LAN.
package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

//go:embed index.html
var indexHTML []byte

const (
	chunk    = 16 * 1024 * 1024 // multipart part size for uploads
	maxCopy  = 5 << 30          // largest object a single CopyObject may copy
	copyPart = 512 << 20        // part size for multipart copies above that
	workers  = 8                // parallel copies per request
)

type server struct {
	s3       *s3.Client
	uploader *manager.Uploader
	endpoint string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve the UI on")
	endpoint := flag.String("endpoint-url", envOr("AWS_ENDPOINT_URL", "http://192.168.1.169:3900"), "S3 endpoint")
	profile := flag.String("profile", "", "AWS profile name from ~/.aws")
	region := flag.String("region", "", "S3 region (default: from env/profile, else garage)")
	auth := flag.String("auth", os.Getenv("GARAGE_UI_AUTH"), "require HTTP basic auth, as user:pass")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := []func(*config.LoadOptions) error{config.WithRetryMaxAttempts(5)}
	if *profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(*profile))
	}
	if *region != "" {
		opts = append(opts, config.WithRegion(*region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		log.Fatalf("loading AWS config: %v", err)
	}
	if cfg.Region == "" {
		cfg.Region = "garage"
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(*endpoint)
		// Garage serves buckets by path, not as <bucket>.<host> subdomains.
		o.UsePathStyle = true
		// Only send checksums S3 requires; avoids issues with non-AWS stores.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	s := &server{
		s3:       client,
		uploader: manager.NewUploader(client, func(u *manager.Uploader) { u.PartSize = chunk }),
		endpoint: *endpoint,
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           guard(*auth, s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("garage-ui on http://%s -> %s (region %s)", *listen, *endpoint, cfg.Region)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// guard adds optional basic auth, and requires a custom header on every
// state-changing request so other web pages can't forge them (a cross-origin
// request with a custom header needs a CORS preflight, which we never grant).
func guard(auth string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != "" {
			user, pass, _ := r.BasicAuth()
			if subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(auth)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="garage-ui"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Garage-UI") == "" {
			http.Error(w, "missing X-Garage-UI header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.Handle("GET /api/config", handler(s.config))
	mux.Handle("GET /api/buckets", handler(s.buckets))
	mux.Handle("GET /api/list", handler(s.list))
	mux.Handle("GET /api/object", handler(s.download))
	mux.Handle("PUT /api/object", handler(s.upload))
	mux.Handle("POST /api/folder", handler(s.mkdir))
	mux.Handle("POST /api/delete", handler(s.delete))
	mux.Handle("POST /api/transfer", handler(s.transfer))
	return mux
}

// --- helpers ----------------------------------------------------------------

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return &httpError{code, fmt.Sprintf(format, args...)}
}

// handler turns a returned error into a JSON {"error": ...} response.
func handler(fn func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := fn(w, r)
		if err == nil {
			return
		}
		code, msg := http.StatusBadGateway, err.Error()
		var he *httpError
		var re *awshttp.ResponseError
		var ae smithy.APIError
		if errors.As(err, &he) {
			code = he.code
		} else if errors.As(err, &re) && re.HTTPStatusCode() >= 400 && re.HTTPStatusCode() < 500 {
			code = re.HTTPStatusCode()
		}
		if errors.As(err, &ae) {
			msg = ae.ErrorCode()
			if m := ae.ErrorMessage(); m != "" {
				msg += ": " + m
			}
		}
		if r.Context().Err() == nil {
			log.Printf("%s %s: %v", r.Method, r.URL, err)
		}
		writeJSON(w, code, map[string]string{"error": msg})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fail(http.StatusBadRequest, "bad request body: %v", err)
	}
	return nil
}

func isNotFound(err error) bool {
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound
}

// escapePath percent-encodes a bucket/key for the x-amz-copy-source header,
// keeping slashes.
func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("/-_.~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *server) listAll(ctx context.Context, bucket, prefix string) ([]types.Object, error) {
	var objs []types.Object
	p := s3.NewListObjectsV2Paginator(s.s3, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		objs = append(objs, page.Contents...)
	}
	return objs, nil
}

// exists reports whether key exists, or for a key ending in "/", whether
// anything is stored under that prefix.
func (s *server) exists(ctx context.Context, bucket, key string) (bool, error) {
	if strings.HasSuffix(key, "/") {
		out, err := s.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &key, MaxKeys: aws.Int32(1)})
		if err != nil {
			return false, err
		}
		return len(out.Contents) > 0, nil
	}
	_, err := s.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key})
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *server) deleteKeys(ctx context.Context, bucket string, keys []string) error {
	for len(keys) > 0 {
		n := min(len(keys), 1000)
		ids := make([]types.ObjectIdentifier, n)
		for i, k := range keys[:n] {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		out, err := s.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &bucket,
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("deleting %s: %s", aws.ToString(e.Key), aws.ToString(e.Message))
		}
		keys = keys[n:]
	}
	return nil
}

// --- handlers ---------------------------------------------------------------

func (s *server) config(w http.ResponseWriter, r *http.Request) error {
	return writeJSON(w, http.StatusOK, map[string]string{"endpoint": s.endpoint})
}

func (s *server) buckets(w http.ResponseWriter, r *http.Request) error {
	out, err := s.s3.ListBuckets(r.Context(), &s3.ListBucketsInput{})
	if err != nil {
		return err
	}
	names := []string{}
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
	}
	return writeJSON(w, http.StatusOK, names)
}

// list returns one page of the folders and files directly under a prefix.
func (s *server) list(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	in := &s3.ListObjectsV2Input{
		Bucket:    aws.String(q.Get("bucket")),
		Prefix:    &prefix,
		Delimiter: aws.String("/"),
	}
	if t := q.Get("token"); t != "" {
		in.ContinuationToken = &t
	}
	out, err := s.s3.ListObjectsV2(r.Context(), in)
	if err != nil {
		return err
	}

	type file struct {
		Key      string    `json:"key"`
		Size     int64     `json:"size"`
		Modified time.Time `json:"modified"`
	}
	res := struct {
		Folders []string `json:"folders"`
		Files   []file   `json:"files"`
		Next    string   `json:"next,omitempty"`
	}{Folders: []string{}, Files: []file{}}
	for _, p := range out.CommonPrefixes {
		res.Folders = append(res.Folders, aws.ToString(p.Prefix))
	}
	for _, o := range out.Contents {
		if aws.ToString(o.Key) == prefix {
			continue // the folder's own marker object
		}
		res.Files = append(res.Files, file{aws.ToString(o.Key), aws.ToInt64(o.Size), aws.ToTime(o.LastModified)})
	}
	if aws.ToBool(out.IsTruncated) {
		res.Next = aws.ToString(out.NextContinuationToken)
	}
	return writeJSON(w, http.StatusOK, res)
}

// download streams an object; ?inline shows it in the browser instead.
// Range requests are passed through so media can seek.
func (s *server) download(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	key := q.Get("key")
	in := &s3.GetObjectInput{Bucket: aws.String(q.Get("bucket")), Key: &key}
	if rg := r.Header.Get("Range"); rg != "" {
		in.Range = &rg
	}
	out, err := s.s3.GetObject(r.Context(), in)
	if err != nil {
		return err
	}
	defer out.Body.Close()

	h := w.Header()
	h.Set("Content-Type", cmpOr(aws.ToString(out.ContentType), "application/octet-stream"))
	if out.ContentLength != nil {
		h.Set("Content-Length", strconv.FormatInt(*out.ContentLength, 10))
	}
	disposition := "attachment"
	if q.Has("inline") {
		disposition = "inline"
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": path.Base(key)}))
	h.Set("Accept-Ranges", "bytes")
	// Stored HTML must not run with this UI's origin.
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	code := http.StatusOK
	if out.ContentRange != nil {
		h.Set("Content-Range", *out.ContentRange)
		code = http.StatusPartialContent
	}
	w.WriteHeader(code)
	io.Copy(w, out.Body)
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// upload streams the request body into bucket/key.
func (s *server) upload(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	key := q.Get("key")
	if key == "" || strings.HasSuffix(key, "/") {
		return fail(http.StatusBadRequest, "invalid key %q", key)
	}
	ct := cmpOr(r.Header.Get("Content-Type"), mime.TypeByExtension(path.Ext(key)))
	_, err := s.uploader.Upload(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(q.Get("bucket")),
		Key:         &key,
		Body:        r.Body,
		ContentType: aws.String(cmpOr(ct, "application/octet-stream")),
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

// mkdir creates an empty "folder/" marker object so empty folders show up.
func (s *server) mkdir(w http.ResponseWriter, r *http.Request) error {
	var req struct{ Bucket, Key string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	key := strings.TrimSuffix(req.Key, "/") + "/"
	if key == "/" {
		return fail(http.StatusBadRequest, "empty folder name")
	}
	_, err := s.s3.PutObject(r.Context(), &s3.PutObjectInput{Bucket: &req.Bucket, Key: &key, Body: strings.NewReader("")})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

// delete removes files, and folders (keys ending in "/") with everything in them.
func (s *server) delete(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Bucket string
		Keys   []string
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var keys []string
	for _, k := range req.Keys {
		switch {
		case k == "" || k == "/":
			return fail(http.StatusBadRequest, "refusing to delete %q", k)
		case strings.HasSuffix(k, "/"):
			objs, err := s.listAll(r.Context(), req.Bucket, k)
			if err != nil {
				return err
			}
			for _, o := range objs {
				keys = append(keys, aws.ToString(o.Key))
			}
		default:
			keys = append(keys, k)
		}
	}
	if err := s.deleteKeys(r.Context(), req.Bucket, keys); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]int{"deleted": len(keys)})
}

type copyJob struct {
	srcBucket, srcKey, dstBucket, dstKey string
	size                                 int64
}

// transfer copies or moves files and folders, possibly across buckets.
// Rename is a move within the same folder. Existing destinations are only
// overwritten with "overwrite": true; otherwise the response is 409.
func (s *server) transfer(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Op    string `json:"op"` // "copy" or "move"
		Items []struct {
			SrcBucket string `json:"srcBucket"`
			SrcKey    string `json:"srcKey"`
			DstBucket string `json:"dstBucket"`
			DstKey    string `json:"dstKey"`
		} `json:"items"`
		Overwrite bool `json:"overwrite"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Op != "copy" && req.Op != "move" {
		return fail(http.StatusBadRequest, "unknown op %q", req.Op)
	}
	ctx := r.Context()

	var jobs []copyJob
	for _, it := range req.Items {
		folder := strings.HasSuffix(it.SrcKey, "/")
		switch {
		case it.SrcKey == "" || it.DstKey == "" || it.DstKey == "/":
			return fail(http.StatusBadRequest, "empty source or destination")
		case folder != strings.HasSuffix(it.DstKey, "/"):
			return fail(http.StatusBadRequest, "%s: source and destination must both be folders or both be files", it.SrcKey)
		case it.SrcBucket == it.DstBucket && it.SrcKey == it.DstKey:
			return fail(http.StatusBadRequest, "%s: source and destination are the same", it.SrcKey)
		case folder && it.SrcBucket == it.DstBucket && strings.HasPrefix(it.DstKey, it.SrcKey):
			return fail(http.StatusBadRequest, "%s: can't copy a folder into itself", it.SrcKey)
		}
		if !req.Overwrite {
			exists, err := s.exists(ctx, it.DstBucket, it.DstKey)
			if err != nil {
				return err
			}
			if exists {
				return fail(http.StatusConflict, "%s already exists in %s", it.DstKey, it.DstBucket)
			}
		}
		if folder {
			objs, err := s.listAll(ctx, it.SrcBucket, it.SrcKey)
			if err != nil {
				return err
			}
			for _, o := range objs {
				key := aws.ToString(o.Key)
				jobs = append(jobs, copyJob{it.SrcBucket, key, it.DstBucket, it.DstKey + strings.TrimPrefix(key, it.SrcKey), aws.ToInt64(o.Size)})
			}
		} else {
			head, err := s.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &it.SrcBucket, Key: &it.SrcKey})
			if err != nil {
				return err
			}
			jobs = append(jobs, copyJob{it.SrcBucket, it.SrcKey, it.DstBucket, it.DstKey, aws.ToInt64(head.ContentLength)})
		}
	}

	if err := s.copyAll(ctx, jobs); err != nil {
		return err
	}
	// Sources are only deleted once every copy has succeeded.
	if req.Op == "move" {
		byBucket := map[string][]string{}
		for _, j := range jobs {
			byBucket[j.srcBucket] = append(byBucket[j.srcBucket], j.srcKey)
		}
		for bucket, keys := range byBucket {
			if err := s.deleteKeys(ctx, bucket, keys); err != nil {
				return err
			}
		}
	}
	return writeJSON(w, http.StatusOK, map[string]int{"count": len(jobs)})
}

func (s *server) copyAll(ctx context.Context, jobs []copyJob) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
		sem      = make(chan struct{}, workers)
	)
	for _, j := range jobs {
		sem <- struct{}{}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := s.copyObject(ctx, j); err != nil {
				once.Do(func() { firstErr = fmt.Errorf("copying %s: %w", j.srcKey, err); cancel() })
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// copyObject copies server side; objects over 5 GiB need a multipart copy.
func (s *server) copyObject(ctx context.Context, j copyJob) error {
	src := aws.String(escapePath(j.srcBucket + "/" + j.srcKey))
	if j.size <= maxCopy {
		_, err := s.s3.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &j.dstBucket, Key: &j.dstKey, CopySource: src})
		return err
	}

	head, err := s.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &j.srcBucket, Key: &j.srcKey})
	if err != nil {
		return err
	}
	mu, err := s.s3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &j.dstBucket, Key: &j.dstKey, ContentType: head.ContentType, Metadata: head.Metadata,
	})
	if err != nil {
		return err
	}
	abort := func(err error) error {
		s.s3.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket: &j.dstBucket, Key: &j.dstKey, UploadId: mu.UploadId,
		})
		return err
	}
	var parts []types.CompletedPart
	for n, off := int32(1), int64(0); off < j.size; n, off = n+1, off+copyPart {
		end := min(off+copyPart, j.size) - 1
		out, err := s.s3.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket: &j.dstBucket, Key: &j.dstKey, UploadId: mu.UploadId, PartNumber: aws.Int32(n),
			CopySource: src, CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", off, end)),
		})
		if err != nil {
			return abort(err)
		}
		parts = append(parts, types.CompletedPart{ETag: out.CopyPartResult.ETag, PartNumber: aws.Int32(n)})
	}
	_, err = s.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &j.dstBucket, Key: &j.dstKey, UploadId: mu.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return abort(err)
	}
	return nil
}
