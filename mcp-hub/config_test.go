package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadConfig(t *testing.T) {
	p := writeConfig(t, `{
	  "servers": [
	    {"name": "vigil", "url": "http://127.0.0.1:8090/mcp", "token_env": "VIGIL_MCP_TOKEN"},
	    {"name": "url-to-bucket", "url": "http://127.0.0.1:8093/mcp", "timeout": "30s"}
	  ],
	  "profiles": {"ops": {"servers": ["vigil"], "deny": ["vigil__get_*"]}}
	}`)
	c, err := loadConfig(p, env(map[string]string{"VIGIL_MCP_TOKEN": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Servers[0].token != "s3cret" || c.Servers[0].timeout != defaultTimeout || c.Servers[1].timeout != 30*time.Second {
		t.Errorf("servers = %+v", c.Servers)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	srv := func(s string) string { return `{"servers": [` + s + `]}` }
	for _, tc := range []struct{ body, want string }{
		{`{"servers": [], "extra": 1}`, "unknown field"},
		{srv(`{"name": "Vigil", "url": "http://x/mcp"}`), "name must be"},
		{srv(`{"name": "a_b", "url": "http://x/mcp"}`), "name must be"},
		{srv(`{"name": "a", "url": "http://x/mcp"}, {"name": "a", "url": "http://y/mcp"}`), "listed twice"},
		{srv(`{"name": "a", "url": "ftp://x/mcp"}`), "url must be"},
		{srv(`{"name": "a", "url": "http://x/mcp", "token_env": "NOPE"}`), "NOPE is not set"},
		{srv(`{"name": "a", "url": "http://x/mcp", "timeout": "11m"}`), "timeout must be"},
		{srv(`{"name": "a", "url": "http://x/mcp", "timeout": "soon"}`), "timeout must be"},
		{`{"servers": [{"name": "a", "url": "http://x/mcp"}], "profiles": {"ops": {"servers": ["b"]}}}`, `unknown server "b"`},
		{`{"servers": [{"name": "a", "url": "http://x/mcp"}], "profiles": {"Ops": {"servers": ["a"]}}}`, "name must be"},
		{`{"servers": [{"name": "a", "url": "http://x/mcp"}], "profiles": {"ops": {"servers": []}}}`, "servers is empty"},
		{`{"servers": [{"name": "a", "url": "http://x/mcp"}], "profiles": {"ops": {"servers": ["a"], "allow": ["a.*"]}}}`, "not a tool name"},
	} {
		_, err := loadConfig(writeConfig(t, tc.body), env(nil))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.body, err, tc.want)
		}
	}
}

func TestProfileShows(t *testing.T) {
	p := ProfileConf{Servers: []string{"vigil", "relay"}, Allow: []string{"vigil__*", "relay__wait"}, Deny: []string{"vigil__get_system_history"}}
	for name, want := range map[string]bool{
		"vigil__list_jobs":          true,
		"vigil__get_system_history": false, // denied
		"relay__wait":               true,
		"relay__publish":            false, // not allowed
	} {
		server, _, _ := strings.Cut(name, "__")
		if got := p.shows(server, name); got != want {
			t.Errorf("%s: shows = %v, want %v", name, got, want)
		}
	}
	if (ProfileConf{Servers: []string{"vigil"}}).shows("lakehouse", "lakehouse__x") {
		t.Error("a profile must not show a server it doesn't list")
	}
}

// The servers.json that deploy.sh installs on the server.
func TestDeployedConfig(t *testing.T) {
	c, err := loadConfig("servers.json", func(string) string { return "token" })
	if err != nil {
		t.Fatal(err)
	}
	oncall, ok := c.Profiles["oncall"]
	if !ok || !oncall.shows("vigil", "vigil__list_events") || oncall.shows("vigil", "vigil__get_system") {
		t.Errorf("oncall = %+v", oncall)
	}
}
