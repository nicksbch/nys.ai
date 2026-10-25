package client

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCutForModel(t *testing.T) {
	if s := CutForModel("short", 8, "x"); s != "short" {
		t.Errorf("cut a short result: %q", s)
	}
	s := CutForModel(strings.Repeat("é", 100), 101, "Ask for less.")
	b, _ := json.Marshal(s)
	if !strings.HasPrefix(s, strings.Repeat("é", 50)+"\n") || !strings.HasSuffix(s, "[cut: 100 more bytes. Ask for less.]") || strings.Contains(string(b), `�`) {
		t.Errorf("cut = %q", s)
	}
}

func TestArgs(t *testing.T) {
	if a, err := ParseArgs(nil); err != nil || len(a) != 0 {
		t.Errorf("ParseArgs(nil) = %v, %v", a, err)
	}
	if _, err := ParseArgs(json.RawMessage(`"x"`)); err == nil {
		t.Error("ParseArgs took a string")
	}
	schema := map[string]any{"type": "object", "required": []any{"url"}, "properties": map[string]any{"url": map[string]any{"type": "string"}}}
	if err := ValidateArgs(schema, map[string]any{"url": "x"}); err != nil {
		t.Error(err)
	}
	if err := ValidateArgs(schema, map[string]any{}); err == nil {
		t.Error("ValidateArgs took arguments without url")
	}
}

func TestResults(t *testing.T) {
	res := &mcp.CallToolResult{StructuredContent: map[string]any{"run_id": "r1", "topic": "url-to-bucket.download"}}
	if s := ResultText(res); s != `{"run_id":"r1","topic":"url-to-bucket.download"}` {
		t.Errorf("ResultText = %s", s)
	}
	if id, topic := JobOf(res); id != "r1" || topic != "url-to-bucket.download" {
		t.Errorf("JobOf = %s, %s", id, topic)
	}
	if id, _ := JobOf(nil); id != "" {
		t.Error("JobOf(nil)")
	}
	ro := &mcp.Tool{Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}
	if !ReadOnly(ro) || ReadOnly(&mcp.Tool{}) {
		t.Error("ReadOnly")
	}
}
