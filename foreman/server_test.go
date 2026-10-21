package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type apiClient struct {
	t   *testing.T
	url string
}

func (c apiClient) do(method, path, body string, auth bool) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.SetBasicAuth("admin", "pw")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func newAPI(t *testing.T) (*harness, apiClient) {
	model := newFakeModel(t, openAI("", [2]string{"submit", goodSubmit}))
	env := newTestEnv(t, model, 0)
	env.e.defs.Types["duckpipe-failure"].OnTopic = ""
	srv := httptest.NewServer(env.e.routes(env.ctx, "admin:pw"))
	t.Cleanup(srv.Close)
	return env, apiClient{t, srv.URL}
}

func TestAPIAuth(t *testing.T) {
	_, c := newAPI(t)
	for _, p := range []string{"/ui", "/api/tasks", "/api/agents", "/api/types"} {
		if code, _ := c.do("GET", p, "", false); code != 401 {
			t.Errorf("%s without a password: %d", p, code)
		}
	}
	if code, _ := c.do("POST", "/api/tasks", `{"type":"duckpipe-failure","input":"x"}`, false); code != 401 {
		t.Errorf("POST without a password: %d", code)
	}
	for _, p := range []string{"/", "/healthz"} {
		if code, _ := c.do("GET", p, "", false); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _ := c.do("GET", "/ui", "", true); code != 200 {
		t.Errorf("/ui with the password: %d", code)
	}
}

func TestAPITaskFlow(t *testing.T) {
	env, c := newAPI(t)

	// Posting needs JSON, so another site's form can't do it.
	req, _ := http.NewRequest("POST", c.url+"/api/tasks", strings.NewReader("type=duckpipe-failure&input=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("admin", "pw")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 415 {
		t.Errorf("form post: %d", resp.StatusCode)
	}
	if code, out := c.do("POST", "/api/tasks", `{"type":"nope","input":"x"}`, true); code != 400 || out["error"] != `no task type "nope"` {
		t.Errorf("unknown type: %d %v", code, out)
	}

	code, out := c.do("POST", "/api/tasks", `{"type":"duckpipe-failure","input":"Why did orders fail?"}`, true)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["task"].(map[string]any)["id"].(string)
	eventually(t, "answered", func() bool { return env.st.get(id).Status == statusAnswered })

	_, out = c.do("GET", "/api/tasks?status=inbox", "", true)
	tasks := out["tasks"].([]any)
	if len(tasks) != 1 || out["total"] != 1.0 || tasks[0].(map[string]any)["input"] != "" {
		t.Errorf("inbox = %v", out)
	}
	if _, out := c.do("GET", "/api/tasks?status=resolved", "", true); len(out["tasks"].([]any)) != 0 {
		t.Errorf("resolved = %v", out)
	}
	if _, out := c.do("GET", "/api/tasks?verdict=none&type=duckpipe-failure&agent=oncall", "", true); len(out["tasks"].([]any)) != 1 {
		t.Errorf("filtered = %v", out)
	}

	_, out = c.do("GET", "/api/tasks/"+id, "", true)
	task := out["task"].(map[string]any)
	if task["input"] != "Why did orders fail?" || strings.Join(toStrings(out["output"]), ",") != "explanation,fix" {
		t.Errorf("task = %v", out)
	}
	runID := task["runs"].([]any)[0].(map[string]any)["id"].(string)
	_, out = c.do("GET", "/api/runs/"+runID, "", true)
	recs := out["records"].([]any)
	if out["task"] != id || len(recs) < 3 || recs[0].(map[string]any)["kind"] != "start" {
		t.Errorf("run = %v", out)
	}
	if code, _ := c.do("GET", "/api/runs/nope", "", true); code != 404 {
		t.Errorf("unknown run: %d", code)
	}

	if code, out := c.do("POST", "/api/tasks/"+id+"/verdict", `{"verdict":"good","note":"right"}`, true); code != 200 ||
		out["task"].(map[string]any)["verdict"] != "good" {
		t.Errorf("verdict: %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/tasks/"+id+"/verdict", `{"verdict":"meh"}`, true); code != 400 {
		t.Errorf("bad verdict: %d", code)
	}
	if code, _ := c.do("POST", "/api/tasks/"+id+"/run", `{"model":"other"}`, true); code != 200 {
		t.Errorf("run again: %d", code)
	}
	eventually(t, "second run", func() bool { got := env.st.get(id); return len(got.Runs) == 2 && got.Status == statusAnswered })
	if code, out := c.do("POST", "/api/tasks/"+id+"/status", `{"status":"resolved"}`, true); code != 200 ||
		out["task"].(map[string]any)["status"] != "resolved" {
		t.Errorf("resolve: %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/tasks/"+id+"/status", `{"status":"dismissed"}`, true); code != 409 {
		t.Errorf("closing twice: %d", code)
	}
	if code, _ := c.do("POST", "/api/tasks/nope/status", `{"status":"resolved"}`, true); code != 404 {
		t.Errorf("unknown task: %d", code)
	}
}

func TestAPIDefinitions(t *testing.T) {
	env, c := newAPI(t)
	tk, _ := env.e.createUserTask(env.ctx, "duckpipe-failure", "x")
	eventually(t, "answered", func() bool { return env.st.get(tk.ID).Status == statusAnswered })

	_, out := c.do("GET", "/api/agents", "", true)
	a := out["agents"].([]any)[0].(map[string]any)
	if a["name"] != "oncall" || a["timeout"] != "10s" || a["counts"].(map[string]any)["answered"] != 1.0 ||
		strings.Join(toStrings(out["models"]), ",") != "local,other" {
		t.Errorf("agents = %v", out)
	}
	env.e.defs.Types["duckpipe-failure"].Every = 0
	_, out = c.do("GET", "/api/types", "", true)
	ty := out["types"].([]any)[0].(map[string]any)
	if ty["name"] != "duckpipe-failure" || ty["agent"] != "oncall" || ty["counts"].(map[string]any)["tasks"] != 1.0 {
		t.Errorf("types = %v", out)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
