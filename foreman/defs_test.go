package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testEnv = map[string]string{"LLAMA_API_KEY": "l", "ANTHROPIC_API_KEY": "a"}

func getenv(k string) string { return testEnv[k] }

// The config that deploy.sh installs on the server.
func TestConfigParses(t *testing.T) {
	d, err := loadDefs("config", getenv)
	if err != nil {
		t.Fatal(err)
	}
	a, tt := d.Agents["oncall"], d.Types["duckpipe-failure"]
	if a == nil || a.Profile != "oncall" || a.Model != "local" || a.MaxSteps != 10 || a.Timeout != 5*time.Minute {
		t.Errorf("agent = %+v", a)
	}
	if tt == nil || tt.OnTopic != "duckpipe.*" || strings.Join(tt.OnTypes, ",") != "failed" || tt.Agent != "oncall" ||
		strings.Join(tt.GroupBy, " ") != "topic data.pipe" || strings.Join(tt.Output, " ") != "explanation fix" {
		t.Errorf("task type = %+v", tt)
	}
	// One on-call task type per part of the pipeline.
	for name, want := range map[string]string{
		"url-to-bucket-failure": "url-to-bucket.download failed | topic data.url",
		"preprocessor-failure":  "preprocessor.* failed | topic data.job",
		"lakeshape-failure":     "lakeshape.* failed | type",
	} {
		tt := d.Types[name]
		if tt == nil {
			t.Errorf("%s: missing", name)
			continue
		}
		got := tt.OnTopic + " " + strings.Join(tt.OnTypes, ",") + " | " + strings.Join(tt.GroupBy, " ")
		if got != want || tt.Agent != "oncall" || strings.Join(tt.Output, " ") != "explanation fix" {
			t.Errorf("%s = %q, agent %s, output %v", name, got, tt.Agent, tt.Output)
		}
	}
	if m := d.Models["local"]; m == nil || m.API != "openai" || m.MaxTokens != 4096 {
		t.Errorf("local = %+v", m)
	}
}

func TestAnthropicModel(t *testing.T) {
	dir := writeDefs(t, "---\nprofile: p\nmodel: claude\n---\nx", "---\nagent: a\n---\ny")
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(`{"models": {"claude": {"api": "anthropic",
		"model": "claude-opus-5-5", "effort": "medium", "key_env": "ANTHROPIC_API_KEY", "max_tokens": 16000}}}`), 0o644)
	d, err := loadDefs(dir, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if m := d.Models["claude"]; m.Effort != "medium" || m.key != "a" || m.MaxTokens != 16000 {
		t.Errorf("claude = %+v", m)
	}
}

// writeDefs makes a config directory holding one agent and one task type.
func writeDefs(t *testing.T, agent, task string) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	os.MkdirAll(filepath.Join(dir, "tasks"), 0o755)
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(`{"models": {"local": {"api": "openai", "url": "http://x/v1", "model": "m"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "agents", "a.md"), []byte(agent), 0o644)
	os.WriteFile(filepath.Join(dir, "tasks", "t.md"), []byte(task), 0o644)
	return dir
}

func TestDefErrors(t *testing.T) {
	agent := "---\nprofile: ops\nmodel: local\n---\nYou help."
	task := "---\nagent: a\n---\nDo it."
	if _, err := loadDefs(writeDefs(t, agent, task), getenv); err != nil {
		t.Fatalf("valid files: %v", err)
	}
	for _, tc := range []struct{ agent, task, want string }{
		{"profile: ops\n---\nx", task, `must start with a "---"`},
		{"---\nprofile: ops\nmodel: local\nx", task, "no closing"},
		{"---\nprofile: ops\nmodel: local\ncolour: red\n---\nx", task, `unknown key "colour"`},
		{"---\nmodel: local\n---\nx", task, "profile is required"},
		{"---\nprofile: ops\nmodel: gpt\n---\nx", task, `model "gpt" is not in models.json`},
		{"---\nprofile: ops\nmodel: local\nmax_steps: 99\n---\nx", task, "max_steps must be"},
		{"---\nprofile: ops\nmodel: local\ntimeout: 2h\n---\nx", task, "timeout must be"},
		{"---\nprofile: ops\nmodel: local\n---\n", task, "prompt"},
		{agent, "---\nagent: b\n---\nx", `agent "b" does not exist`},
		{agent, "---\nagent: a\non_event: duckpipe.*\n---\nx", "on_event must be"},
		{agent, "---\nagent: a\nevery: 10s\n---\nx", "every must be"},
		{agent, "---\nagent: a\ngroup_by: data\n---\nx", "group_by"},
		{agent, "---\nagent: a\noutput: Fix\n---\nx", "output"},
		{agent, "---\nagent: a\noutput: fix fix\n---\nx", "listed twice"},
	} {
		_, err := loadDefs(writeDefs(t, tc.agent, tc.task), getenv)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("agent %q task %q: got %v, want %q", tc.agent, tc.task, err, tc.want)
		}
	}
}

func TestModelErrors(t *testing.T) {
	for _, tc := range []struct{ conf, want string }{
		{`{"api": "openai", "model": "m"}`, "url must be"},
		{`{"api": "openai", "url": "http://x", "model": "m", "effort": "low"}`, "only apply to the anthropic"},
		{`{"api": "anthropic", "model": "m"}`, "key_env is required"},
		{`{"api": "anthropic", "model": "m", "key_env": "UNSET"}`, "UNSET is not set"},
		{`{"api": "anthropic", "model": "m", "key_env": "ANTHROPIC_API_KEY", "effort": "huge"}`, "effort must be"},
		{`{"api": "bard", "model": "m"}`, "api must be"},
		{`{"api": "openai", "url": "http://x", "model": "m", "colour": 1}`, "unknown field"},
	} {
		p := filepath.Join(t.TempDir(), "models.json")
		os.WriteFile(p, []byte(`{"models": {"x": `+tc.conf+`}}`), 0o644)
		_, err := loadModels(p, getenv)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.conf, err, tc.want)
		}
	}
}
