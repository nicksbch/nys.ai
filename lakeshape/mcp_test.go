package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	relay "relay/client"
)

// fakeRelay records the events published to it.
type fakeRelay struct {
	mu     sync.Mutex
	events []relay.Event
}

func (f *fakeRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var e relay.Event
	json.NewDecoder(r.Body).Decode(&e)
	f.mu.Lock()
	f.events = append(f.events, e)
	e.ID = uint64(len(f.events))
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(e)
}

func (f *fakeRelay) published() []relay.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relay.Event(nil), f.events...)
}

type tokenTransport struct{ token string }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

// testBoard is the board of a service that loaded testdata/shop, with
// relay at bus (nil for none).
func testBoard(t *testing.T, bus *relay.Client) (*board, *httptest.Server) {
	t.Helper()
	p, err := loadProject("testdata/shop", target{name: "prod"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := newBoard(bus)
	jobs := map[string]*job{}
	var order []string
	for _, j := range p.jobs {
		jobs[j.Name] = j
		order = append(order, j.Name)
	}
	b.setJobs(order, jobs)
	srv := httptest.NewServer(b.handler("mcp-token"))
	t.Cleanup(srv.Close)
	return b, srv
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp",
		HTTPClient: &http.Client{Transport: tokenTransport{token}}, DisableStandaloneSSE: true}, nil)
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, out)
	}
	return res
}

func TestMCPListJobs(t *testing.T) {
	b, srv := testBoard(t, nil)
	last := time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC)
	b.setLast(map[string]time.Time{"nightly": last})
	finished := b.start([]string{"orders"})

	s, err := connect(t, srv.URL, "mcp-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	for _, x := range tools.Tools {
		readOnly := x.Annotations != nil && x.Annotations.ReadOnlyHint
		if readOnly != (x.Name == "list_jobs") || (x.Name == "run_job" && x.Meta["wait"] != "2h") {
			t.Errorf("tool = %+v", x)
		}
	}

	var out jobsOut
	call(t, s, "list_jobs", nil, &out)
	if len(out.Jobs) != 2 {
		t.Fatalf("jobs = %+v", out.Jobs)
	}
	orders, nightly := out.Jobs[0], out.Jobs[1]
	if orders.Name != "orders" || !orders.Running || orders.LastRun == nil || len(orders.On) != 1 || orders.On[0].Topic != "duckpipe.ingest" {
		t.Errorf("orders = %+v", orders)
	}
	if nightly.Name != "nightly" || nightly.Running || nightly.Schedule != "0 3 * * *" || nightly.LastRun == nil || !nightly.LastRun.Equal(last) {
		t.Errorf("nightly = %+v", nightly)
	}

	finished()
	call(t, s, "list_jobs", nil, &out)
	if out.Jobs[0].Running {
		t.Error("orders still running after its run finished")
	}
}

func TestMCPRunJob(t *testing.T) {
	rel := &fakeRelay{}
	rs := httptest.NewServer(rel)
	defer rs.Close()
	_, srv := testBoard(t, &relay.Client{URL: rs.URL, HTTP: &http.Client{}})
	s, err := connect(t, srv.URL, "mcp-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var out runJobOut
	if res := call(t, s, "run_job", map[string]any{"job": "nightly", "full_refresh": true}, &out); res.IsError {
		t.Fatalf("run_job: %+v", res.Content)
	}
	evs := rel.published()
	if len(evs) != 1 || evs[0].Topic != "lakeshape.trigger" || evs[0].Type != "requested" || evs[0].RunID != out.RunID || out.Topic != "lakeshape.run" {
		t.Fatalf("result %+v, published %+v", out, evs)
	}
	if data, _ := evs[0].Data.(map[string]any); data["job"] != "nightly" || data["full_refresh"] != true {
		t.Errorf("data = %v", evs[0].Data)
	}

	if res := call(t, s, "run_job", map[string]any{"job": "nope"}, nil); !res.IsError {
		t.Errorf("unknown job: %+v", res)
	}
	if n := len(rel.published()); n != 1 {
		t.Errorf("%d events published, want 1", n)
	}
}

func TestMCPNoRelay(t *testing.T) {
	_, srv := testBoard(t, nil)
	s, err := connect(t, srv.URL, "mcp-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if res := call(t, s, "run_job", map[string]any{"job": "nightly"}, nil); !res.IsError {
		t.Errorf("run_job without relay: %+v", res)
	}
}

func TestMCPAuth(t *testing.T) {
	_, srv := testBoard(t, nil)
	if _, err := connect(t, srv.URL, "wrong"); err == nil {
		t.Error("connected with a wrong token")
	}
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %d", resp.StatusCode)
	}
}
