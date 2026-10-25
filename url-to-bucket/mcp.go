package main

// The MCP tools at /mcp, so an agent can ask for a download through mcp-hub
// (url-to-bucket__download). download publishes the same "requested" event
// as the page and returns the run's ID at once; the download reports on
// relay as any other. list_buckets, list_objects and suggest_key say where
// downloads can go, what is already there, and where a new one belongs;
// list_downloads is the page's list of recent downloads.
// See SPEC-mcp.md.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerAuth requires "Authorization: Bearer <token>".
func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type downloadIn struct {
	URL         string `json:"url" jsonschema:"the http or https URL to download"`
	Bucket      string `json:"bucket" jsonschema:"the bucket to write to"`
	Key         string `json:"key,omitempty" jsonschema:"the object key; ending in / puts the file under that prefix with its own name; empty uses the file's name"`
	ContentType string `json:"content_type,omitempty" jsonschema:"the Content-Type to store; empty uses the server's"`
}

type bucketsOut struct {
	Buckets []string `json:"buckets"`
}

const maxListed = 200 // folders and files list_objects returns

type objectsIn struct {
	Bucket string `json:"bucket" jsonschema:"the bucket to look in"`
	Prefix string `json:"prefix,omitempty" jsonschema:"the folder to list, e.g. uk/crime/; empty for the top of the bucket"`
}

type suggestIn struct {
	Bucket string `json:"bucket" jsonschema:"the bucket the file will go to"`
	URL    string `json:"url" jsonschema:"the URL to download"`
}

type suggestOut struct {
	File        string       `json:"file"`    // the file's own name, from the URL
	Pattern     string       `json:"pattern"` // its name with digits as #
	Suggestions []suggestion `json:"suggestions"`
	Scanned     int          `json:"scanned"`
	Truncated   bool         `json:"truncated"` // the bucket has more than maxScanned objects; only those were looked at
}

type object struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type objectsOut struct {
	Bucket    string   `json:"bucket"`
	Prefix    string   `json:"prefix"`
	Folders   []string `json:"folders"`
	Files     []object `json:"files"`
	Truncated bool     `json:"truncated"` // more than maxListed at this level
}

const defaultListed = 20 // downloads list_downloads returns by default

type downloadsIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many downloads to return, newest first; default 20, at most 100"`
}

type downloadsOut struct {
	Downloads []download `json:"downloads"`
}

type downloadOut struct {
	RunID string `json:"run_id"`
	Topic string `json:"topic"`
}

func (u *ui) mcpHandler() http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "url-to-bucket", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "download",
		Description: "Download a URL into a bucket. key is either the full object key (e.g. uk/crime/2023-08.zip) " +
			"or a folder ending with / (e.g. uk/crime/), and then the file keeps its own name; empty puts it at the top " +
			"of the bucket. Never end a key with the file's name followed by /. It returns at once with the run's " +
			"run_id; the download then reports started, and ok or failed, on relay topic " + topic + " with that run_id. " +
			"Don't retry a URL that failed with a 4xx; it will fail the same way.",
		Meta: mcp.Meta{"wait": "2h"}}, u.toolDownload)
	mcp.AddTool(s, &mcp.Tool{Name: "list_buckets", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "The buckets downloads can go to: the bucket argument of download must be one of them."}, u.toolBuckets)
	mcp.AddTool(s, &mcp.Tool{Name: "list_objects", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "One level of a bucket, like ls: the folders and the files (key, size, last modified) directly under " +
			"prefix. Start with an empty prefix and go down folder by folder, to see where files like the one you " +
			"want to download already are."}, u.toolObjects)
	mcp.AddTool(s, &mcp.Tool{Name: "suggest_key", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "Where a download belongs: the folders that already hold files named like the URL's file " +
			"(same extension, digits in the same places), best first, each with the full key to download to and " +
			"examples of what is there. Use the first suggestion's key for download. No suggestions means nothing " +
			"similar is in the bucket: ask the person where to put it."}, u.toolSuggest)
	mcp.AddTool(s, &mcp.Tool{Name: "list_downloads", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "Recent downloads, newest first, as the page lists them: run_id, state (queued, running, ok, " +
			"failed), url, bucket, key, bytes so far or in all, total and rate while running, and the error of a " +
			"failed one."}, u.toolDownloads)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
}

func (u *ui) toolBuckets(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, bucketsOut, error) {
	names, err := u.bucketNames(ctx)
	if err != nil {
		return nil, bucketsOut{}, err
	}
	return nil, bucketsOut{Buckets: names}, nil
}

func (u *ui) toolObjects(ctx context.Context, req *mcp.CallToolRequest, in objectsIn) (*mcp.CallToolResult, objectsOut, error) {
	if in.Bucket == "" {
		return nil, objectsOut{}, errors.New("bucket is required")
	}
	prefix := strings.TrimPrefix(in.Prefix, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/" // a folder, never part of a name
	}
	res, err := u.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(in.Bucket), Prefix: aws.String(prefix),
		Delimiter: aws.String("/"), MaxKeys: aws.Int32(maxListed)})
	if err != nil {
		return nil, objectsOut{}, err
	}
	out := objectsOut{Bucket: in.Bucket, Prefix: prefix, Folders: []string{}, Files: []object{}, Truncated: aws.ToBool(res.IsTruncated)}
	for _, p := range res.CommonPrefixes {
		out.Folders = append(out.Folders, aws.ToString(p.Prefix))
	}
	for _, o := range res.Contents {
		if key := aws.ToString(o.Key); key != prefix { // a folder marker, not a file
			out.Files = append(out.Files, object{Key: key, Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified)})
		}
	}
	return nil, out, nil
}

func (u *ui) toolSuggest(ctx context.Context, req *mcp.CallToolRequest, in suggestIn) (*mcp.CallToolResult, suggestOut, error) {
	if in.Bucket == "" {
		return nil, suggestOut{}, errors.New("bucket is required")
	}
	file := defaultKey(in.URL)
	out := suggestOut{File: file, Pattern: shape(file)}
	var objs []object
	p := s3.NewListObjectsV2Paginator(u.s3, &s3.ListObjectsV2Input{Bucket: aws.String(in.Bucket)})
	for p.HasMorePages() && len(objs) < maxScanned {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, suggestOut{}, err
		}
		for _, o := range page.Contents {
			objs = append(objs, object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified)})
		}
	}
	out.Scanned, out.Truncated = len(objs), p.HasMorePages()
	out.Suggestions = suggest(objs, file)
	return nil, out, nil
}

func (u *ui) toolDownloads(ctx context.Context, req *mcp.CallToolRequest, in downloadsIn) (*mcp.CallToolResult, downloadsOut, error) {
	list, err := u.downloadList(ctx)
	if err != nil {
		return nil, downloadsOut{}, err
	}
	n := in.Limit
	if n <= 0 {
		n = defaultListed
	}
	if len(list) > n {
		list = list[:n]
	}
	return nil, downloadsOut{Downloads: list}, nil
}

func (u *ui) toolDownload(ctx context.Context, req *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, downloadOut, error) {
	data, _ := json.Marshal(in)
	runID, _, err := u.publishRequest(ctx, data)
	if err != nil {
		return nil, downloadOut{}, err
	}
	return nil, downloadOut{RunID: runID, Topic: topic}, nil
}
