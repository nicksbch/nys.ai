package chatapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigErrors(t *testing.T) {
	for _, tc := range []struct{ models, want string }{
		{`{"default": "a", "models": {}}`, "no models"},
		{`{"default": "b", "models": {"a": {"url": "http://x/v1", "model": "m"}}}`, "default"},
		{`{"default": "a", "models": {"a": {"url": "x", "model": "m"}}}`, "url"},
		{`{"default": "a", "models": {"a": {"url": "http://x/v1", "model": "m", "key_env": "K"}}}`, "K is not set (put it in /etc/x.env)"},
		{`{"default": "a", "models": {"a": {"url": "http://x/v1", "model": "m"}}, "extra": 1}`, "unknown field"},
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "models.json"), []byte(tc.models), 0o644)
		os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("hi"), 0o644)
		if _, err := LoadConfig(dir, func(string) string { return "" }, "/etc/x.env"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.models, err, tc.want)
		}
	}
}

func TestArgsJSON(t *testing.T) {
	if string(ArgsJSON("")) != "{}" || string(ArgsJSON(`{"a":1}`)) != `{"a":1}` || string(ArgsJSON(`{bad`)) != `"{bad"` {
		t.Errorf("ArgsJSON")
	}
}

// The model's tool calls get new, unique ids, and the key is sent.
func TestComplete(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices": [{"finish_reason": "tool_calls", "message": {"role": "assistant", "content": " hi ",
			"tool_calls": [{"id": "x", "type": "function", "function": {"name": "ping", "arguments": ""}},
			               {"id": "x", "type": "function", "function": {"name": "ping", "arguments": "{\"a\":1}"}}]}}]}`)
	}))
	defer srv.Close()
	m := &Model{URL: srv.URL + "/v1", Model: "m", KeyEnv: "K"}
	if err := m.check(func(string) string { return "secret" }, ""); err != nil {
		t.Fatal(err)
	}
	r, msg, err := Complete(context.Background(), m, "sys", []Msg{{Role: "user", Content: "go"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer secret" || r.Text != "hi" || len(r.Calls) != 2 || r.Calls[0].ID == r.Calls[1].ID ||
		msg.ToolCalls[0].ID != r.Calls[0].ID || string(r.Calls[0].Args) != "{}" {
		b, _ := json.Marshal(r)
		t.Errorf("auth %q, reply %s", auth, b)
	}
	if err := TestTools(context.Background(), m); err != nil {
		t.Errorf("TestTools: %v", err)
	}
}
