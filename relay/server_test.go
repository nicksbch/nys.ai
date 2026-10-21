package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relay/client"
)

const testToken = "secret"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	st, err := openStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := openConsumers(filepath.Join(dir, "consumers.json"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer((&server{token: testToken, store: st, consumers: cons}).routes())
	t.Cleanup(func() { ts.Close(); st.close() })
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp.StatusCode
}

func TestAuthAndUsage(t *testing.T) {
	ts := newTestServer(t)
	resp, _ := http.Get(ts.URL + "/v1/topics")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("relay wait")) {
		t.Fatalf("GET /: %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/ui")
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !bytes.Contains(b, []byte("v1/events")) {
		t.Fatalf("GET /ui: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestPublishAndRead(t *testing.T) {
	ts := newTestServer(t)
	var e Event
	if code := do(t, ts, "POST", "/v1/events", `{"topic":"a.download","type":"started","run_id":"r1"}`, &e); code != 201 || e.ID != 1 {
		t.Fatalf("publish: %d %+v", code, e)
	}
	do(t, ts, "POST", "/v1/events", `{"topic":"a.download","type":"ok","run_id":"r1","data":{"n":1}}`, nil)
	do(t, ts, "POST", "/v1/events", `{"topic":"b.other","type":"ok"}`, nil)
	if code := do(t, ts, "POST", "/v1/events", `{"topic":"a.download","type":"ok","key":"k"}`, &e); code != 201 {
		t.Fatalf("keyed publish: %d", code)
	}
	if code := do(t, ts, "POST", "/v1/events", `{"topic":"a.download","type":"ok","key":"k"}`, &e); code != 200 || e.ID != 4 {
		t.Fatalf("repeated key: %d %+v", code, e)
	}
	if code := do(t, ts, "POST", "/v1/events", `{"topic":"a.b","type":"ok","id":9}`, nil); code != 400 {
		t.Fatalf("client-set id: %d", code)
	}

	var r eventsResponse
	do(t, ts, "GET", "/v1/events?after=0&topic=a.*&type=ok", "", &r)
	if len(r.Events) != 2 || r.Events[0].ID != 2 || r.Next != 4 {
		t.Fatalf("filtered read: %+v", r)
	}
	do(t, ts, "GET", "/v1/events?after=0&limit=1", "", &r)
	if len(r.Events) != 1 || r.Next != 1 {
		t.Fatalf("limit: %+v", r)
	}
	do(t, ts, "GET", "/v1/events?last=2", "", &r)
	if len(r.Events) != 2 || r.Events[0].ID != 3 || r.Events[1].ID != 4 {
		t.Fatalf("last: %+v", r)
	}
	var topics []topicInfo
	do(t, ts, "GET", "/v1/topics", "", &topics)
	if len(topics) != 2 || topics[0].Topic != "a.download" || topics[0].Count != 3 || len(topics[0].Types) != 2 {
		t.Fatalf("topics: %+v", topics)
	}
}

func TestLongPoll(t *testing.T) {
	ts := newTestServer(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		bus := &client.Client{URL: ts.URL, Token: testToken}
		bus.Publish(context.Background(), client.Event{Topic: "a.b", Type: "ok"})
	}()
	start := time.Now()
	var r eventsResponse
	do(t, ts, "GET", "/v1/events?wait=5s", "", &r)
	if len(r.Events) != 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("long-poll: %+v after %s", r, time.Since(start))
	}

	start = time.Now()
	do(t, ts, "GET", "/v1/events?wait=300ms", "", &r)
	if len(r.Events) != 0 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("long-poll timeout: %+v after %s", r, time.Since(start))
	}
}

func TestConsumers(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "POST", "/v1/events", `{"topic":"a.b","type":"ok"}`, nil) // before the consumer: skipped

	var v consumerView
	if code := do(t, ts, "PUT", "/v1/consumers/idx", `{"topic":"a.*","type":"ok"}`, &v); code != 200 || v.Cursor != 1 {
		t.Fatalf("create: %d %+v", code, v)
	}
	do(t, ts, "POST", "/v1/events", `{"topic":"a.b","type":"started"}`, nil)
	do(t, ts, "POST", "/v1/events", `{"topic":"a.b","type":"ok"}`, nil)
	do(t, ts, "POST", "/v1/events", `{"topic":"a.c","type":"ok"}`, nil)

	var r eventsResponse
	do(t, ts, "GET", "/v1/consumers/idx/next", "", &r)
	if len(r.Events) != 1 || r.Events[0].ID != 3 {
		t.Fatalf("next: %+v", r)
	}
	// Not acked: delivered again.
	do(t, ts, "GET", "/v1/consumers/idx/next", "", &r)
	if len(r.Events) != 1 || r.Events[0].ID != 3 {
		t.Fatalf("redelivery: %+v", r)
	}
	if code := do(t, ts, "POST", "/v1/consumers/idx/ack", `{"id":3}`, &v); code != 200 || v.Cursor != 3 || v.Backlog != 1 {
		t.Fatalf("ack: %d %+v", code, v)
	}
	do(t, ts, "GET", "/v1/consumers/idx/next", "", &r)
	if len(r.Events) != 1 || r.Events[0].ID != 4 {
		t.Fatalf("next after ack: %+v", r)
	}
	if code := do(t, ts, "POST", "/v1/consumers/idx/ack", `{"id":99}`, nil); code != 400 {
		t.Fatalf("ack beyond newest: %d", code)
	}

	// Updating the filter keeps the cursor; from=earliest resets it.
	do(t, ts, "PUT", "/v1/consumers/idx", `{"topic":"*"}`, &v)
	if v.Cursor != 3 || v.Backlog != 1 {
		t.Fatalf("update: %+v", v)
	}
	do(t, ts, "PUT", "/v1/consumers/idx", `{"topic":"*","from":"earliest"}`, &v)
	if v.Cursor != 0 || v.Backlog != 4 {
		t.Fatalf("from earliest: %+v", v)
	}

	var list []consumerView
	do(t, ts, "GET", "/v1/consumers", "", &list)
	if len(list) != 1 || list[0].Name != "idx" {
		t.Fatalf("list: %+v", list)
	}
	if code := do(t, ts, "DELETE", "/v1/consumers/idx", "", nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code := do(t, ts, "GET", "/v1/consumers/idx/next", "", nil); code != 404 {
		t.Fatalf("next on deleted: %d", code)
	}
}
