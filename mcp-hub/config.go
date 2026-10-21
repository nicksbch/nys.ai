package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	defaultTimeout = 60 * time.Second
	maxTimeout     = 10 * time.Minute
)

var (
	serverNameRE  = regexp.MustCompile(`^[a-z0-9-]{1,20}$`)
	profileNameRE = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	toolNameRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`) // what the Claude API accepts
	patternRE     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}\*?$|^\*$`)
)

// Config is servers.json: the upstream MCP servers and the profiles that
// show a subset of their tools.
type Config struct {
	Servers  []ServerConf           `json:"servers"`
	Profiles map[string]ProfileConf `json:"profiles"`
}

type ServerConf struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	TokenEnv string `json:"token_env,omitempty"`
	Timeout  string `json:"timeout,omitempty"`

	token   string        // the value of TokenEnv
	timeout time.Duration // Timeout parsed, or defaultTimeout
}

type ProfileConf struct {
	Servers []string `json:"servers"`
	Allow   []string `json:"allow,omitempty"`
	Deny    []string `json:"deny,omitempty"`
}

// loadConfig reads and checks servers.json. Upstream tokens come from the
// environment (getenv), never from the file.
func loadConfig(path string, getenv func(string) string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.check(getenv); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) check(getenv func(string) string) error {
	seen := map[string]bool{}
	for i := range c.Servers {
		s := &c.Servers[i]
		if !serverNameRE.MatchString(s.Name) {
			return fmt.Errorf("server %q: name must be 1-20 characters of a-z 0-9 -", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("server %q is listed twice", s.Name)
		}
		seen[s.Name] = true
		if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("server %q: url must be an http:// or https:// URL", s.Name)
		}
		if s.TokenEnv != "" {
			if s.token = getenv(s.TokenEnv); s.token == "" {
				return fmt.Errorf("server %q: environment variable %s is not set (put it in /etc/mcp-hub.env)", s.Name, s.TokenEnv)
			}
		}
		s.timeout = defaultTimeout
		if s.Timeout != "" {
			d, err := time.ParseDuration(s.Timeout)
			if err != nil || d <= 0 || d > maxTimeout {
				return fmt.Errorf("server %q: timeout must be a duration such as 30s, at most %s", s.Name, maxTimeout)
			}
			s.timeout = d
		}
	}
	for name, p := range c.Profiles {
		if !profileNameRE.MatchString(name) {
			return fmt.Errorf("profile %q: name must be 1-64 characters of a-z 0-9 -", name)
		}
		if len(p.Servers) == 0 {
			return fmt.Errorf("profile %q: servers is empty", name)
		}
		for _, s := range p.Servers {
			if !seen[s] {
				return fmt.Errorf("profile %q: unknown server %q", name, s)
			}
		}
		for _, pat := range slices.Concat(p.Allow, p.Deny) {
			if !patternRE.MatchString(pat) {
				return fmt.Errorf("profile %q: %q is not a tool name or a name ending in *", name, pat)
			}
		}
	}
	return nil
}

// shows reports whether profile p shows the hub tool name from server.
func (p ProfileConf) shows(server, name string) bool {
	if !slices.Contains(p.Servers, server) {
		return false
	}
	if len(p.Allow) > 0 && !slices.ContainsFunc(p.Allow, func(pat string) bool { return match(pat, name) }) {
		return false
	}
	return !slices.ContainsFunc(p.Deny, func(pat string) bool { return match(pat, name) })
}

func match(pattern, name string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return pattern == name
}
