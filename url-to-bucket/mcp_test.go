package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	relay "relay/client"
)

// publishedRelay records the events published to it.
type publishedRelay struct {
	mu     sync.Mutex
	events []relay.Event
}

func (p *publishedRelay) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e relay.Event
		json.NewDecoder(r.Body).Decode(&e)
		p.mu.Lock()
		p.events = append(p.events, e)
		e.ID = uint64(len(p.events))
		p.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(e)
	})
}

func (p *publishedRelay) published() []relay.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]relay.Event(nil), p.events...)
}

type tokenTransport struct{ token string }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func mcpServer(t *testing.T) (*publishedRelay, *httptest.Server) {
	rel := &publishedRelay{}
	rs := httptest.NewServer(rel.handler())
	t.Cleanup(rs.Close)
	// S3 answers ListBuckets only.
	fakeS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>home</Name></Bucket><Bucket><Name>isos</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
	}))
	t.Cleanup(fakeS3.Close)
	client := s3.New(s3.Options{BaseEndpoint: aws.String(fakeS3.URL), Region: "garage", UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("k", "s", "")})
	u := &ui{bus: &relay.Client{URL: rs.URL, HTTP: &http.Client{}}, s3: client, active: map[string]*progress{}}
	srv := httptest.NewServer(u.handler("admin:pw", "mcp-token"))
	t.Cleanup(srv.Close)
	return rel, srv
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp",
		HTTPClient: &http.Client{Transport: tokenTransport{token}}, DisableStandaloneSSE: true}, nil)
}

func TestMCPDownload(t *testing.T) {
	rel, srv := mcpServer(t)
	s, err := connect(t, srv.URL, "mcp-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tools, err := s.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	var tool *mcp.Tool
	for _, x := range tools.Tools {
		if x.Name == "download" {
			tool = x
		} else if x.Name != "list_buckets" || x.Annotations == nil || !x.Annotations.ReadOnlyHint {
			t.Errorf("tool = %+v", x)
		}
	}
	if tool == nil || (tool.Annotations != nil && tool.Annotations.ReadOnlyHint) || tool.Meta["wait"] != "2h" {
		t.Errorf("tool = %+v", tool)
	}

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "download",
		Arguments: map[string]any{"url": "https://x.org/a.iso", "bucket": "home", "key": "isos/"}})
	if err != nil || res.IsError {
		t.Fatalf("call = %+v, %v", res, err)
	}
	out, _ := res.StructuredContent.(map[string]any)
	evs := rel.published()
	if len(evs) != 1 || evs[0].Topic != topic || evs[0].Type != "requested" || out["run_id"] != evs[0].RunID || out["topic"] != topic {
		t.Fatalf("result %v, published %+v", out, evs)
	}
	var data map[string]any
	b, _ := json.Marshal(evs[0].Data)
	json.Unmarshal(b, &data)
	if data["url"] != "https://x.org/a.iso" || data["bucket"] != "home" || data["key"] != "isos/" {
		t.Errorf("data = %v", data)
	}

	res, err = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_buckets"})
	if err != nil || res.IsError {
		t.Fatalf("list_buckets = %+v, %v", res, err)
	}
	if b, _ := json.Marshal(res.StructuredContent); string(b) != `{"buckets":["home","isos"]}` {
		t.Errorf("list_buckets = %s", b)
	}

	// A request the page would refuse is a tool error, and nothing is published.
	res, err = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "download",
		Arguments: map[string]any{"url": "file:///etc/passwd", "bucket": "home"}})
	if err != nil || !res.IsError {
		t.Fatalf("bad url: %+v, %v", res, err)
	}
	if n := len(rel.published()); n != 1 {
		t.Errorf("%d events published, want 1", n)
	}
}

func TestMCPAuth(t *testing.T) {
	_, srv := mcpServer(t)
	if _, err := connect(t, srv.URL, "wrong"); err == nil {
		t.Error("connected with a wrong token")
	}
	// The page's password doesn't open /mcp.
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{}`))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("basic auth on /mcp: %d", resp.StatusCode)
	}
	// The page still needs its password.
	resp, _ = http.Get(srv.URL + "/api/downloads")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("page without a password: %d", resp.StatusCode)
	}
}
