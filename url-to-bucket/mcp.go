package main

// The MCP tools at /mcp, so an agent can ask for a download through mcp-hub
// (url-to-bucket__download). download publishes the same "requested" event
// as the page and returns the run's ID at once; the download reports on
// relay as any other. list_buckets and list_objects say where downloads
// can go and what is already there. See SPEC-mcp.md.

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

type downloadOut struct {
	RunID string `json:"run_id"`
	Topic string `json:"topic"`
}

func (u *ui) mcpHandler() http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "url-to-bucket", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "download",
		Description: "Download a URL into a bucket. It returns at once with the run's run_id; the download " +
			"then reports started, and ok or failed, on relay topic " + topic + " with that run_id. " +
			"Don't retry a URL that failed with a 4xx; it will fail the same way.",
		Meta: mcp.Meta{"wait": "2h"}}, u.toolDownload)
	mcp.AddTool(s, &mcp.Tool{Name: "list_buckets", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "The buckets downloads can go to: the bucket argument of download must be one of them."}, u.toolBuckets)
	mcp.AddTool(s, &mcp.Tool{Name: "list_objects", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "One level of a bucket, like ls: the folders and the files (key, size, last modified) directly under " +
			"prefix. Start with an empty prefix and go down folder by folder, to see where files like the one you " +
			"want to download already are."}, u.toolObjects)
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

func (u *ui) toolDownload(ctx context.Context, req *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, downloadOut, error) {
	data, _ := json.Marshal(in)
	runID, _, err := u.publishRequest(ctx, data)
	if err != nil {
		return nil, downloadOut{}, err
	}
	return nil, downloadOut{RunID: runID, Topic: topic}, nil
}
