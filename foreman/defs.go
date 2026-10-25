package main

// Agents (who does the work) and task types (what to do, and when) are
// Markdown files with flat "key: value" front matter. Models live in
// models.json. All of it is read at startup and checked strictly.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxSteps = 10
	maxMaxSteps     = 50
	defaultTimeout  = 5 * time.Minute
	maxTimeout      = 30 * time.Minute
)

var (
	defNameRE    = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	fieldNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	topicFiltRE  = regexp.MustCompile(`^[a-z0-9_*.,-]+$`)
	eventTypesRE = regexp.MustCompile(`^[A-Za-z0-9_.,-]+$`)
	groupKeyRE   = regexp.MustCompile(`^(topic|type|run_id|data\.[A-Za-z0-9_]{1,64})$`)
	hubToolRE    = regexp.MustCompile(`^[a-z0-9-]+__[a-z0-9_]+$`)
)

type Agent struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Profile     string        `json:"profile"` // mcp-hub profile; "all" is plain /mcp
	Model       string        `json:"model"`
	MaxSteps    int           `json:"max_steps"`
	Timeout     time.Duration `json:"-"`
	Prompt      string        `json:"prompt"`
}

type TaskType struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	OnTopic     string        `json:"on_topic,omitempty"` // relay topic filter; "" when not event-triggered
	OnTypes     []string      `json:"on_types,omitempty"` // relay event types
	Every       time.Duration `json:"-"`
	Agent       string        `json:"agent"`
	GroupBy     []string      `json:"group_by,omitempty"`
	Output      []string      `json:"output"`            // fields of the result, submitted by the agent
	Actions     []string      `json:"actions,omitempty"` // hub tools that aren't read-only, proposed for your approval
	Prompt      string        `json:"prompt"`
}

type ModelConf struct {
	API       string `json:"api"` // "openai" or "anthropic"
	URL       string `json:"url,omitempty"`
	Model     string `json:"model"`
	KeyEnv    string `json:"key_env,omitempty"`
	MaxTokens int64  `json:"max_tokens,omitempty"`
	Effort    string `json:"effort,omitempty"`    // anthropic only
	Fallbacks *bool  `json:"fallbacks,omitempty"` // anthropic only; default true

	key string // the value of KeyEnv
}

// Defs is everything under the config directory.
type Defs struct {
	Agents map[string]*Agent
	Types  map[string]*TaskType
	Models map[string]*ModelConf
}

// loadDefs reads <dir>/models.json, <dir>/agents/*.md and <dir>/tasks/*.md.
func loadDefs(dir string, getenv func(string) string) (*Defs, error) {
	d := &Defs{Agents: map[string]*Agent{}, Types: map[string]*TaskType{}}
	var err error
	if d.Models, err = loadModels(filepath.Join(dir, "models.json"), getenv); err != nil {
		return nil, err
	}
	if err := eachDef(filepath.Join(dir, "agents"), func(name string, fm map[string]string, body string) error {
		a, err := parseAgent(name, fm, body)
		d.Agents[name] = a
		return err
	}); err != nil {
		return nil, err
	}
	if err := eachDef(filepath.Join(dir, "tasks"), func(name string, fm map[string]string, body string) error {
		t, err := parseTaskType(name, fm, body)
		d.Types[name] = t
		return err
	}); err != nil {
		return nil, err
	}
	for _, a := range d.Agents {
		if d.Models[a.Model] == nil {
			return nil, fmt.Errorf("agents/%s.md: model %q is not in models.json", a.Name, a.Model)
		}
	}
	for _, t := range d.Types {
		if d.Agents[t.Agent] == nil {
			return nil, fmt.Errorf("tasks/%s.md: agent %q does not exist", t.Name, t.Agent)
		}
	}
	return d, nil
}

func loadModels(path string, getenv func(string) string) (map[string]*ModelConf, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Models map[string]*ModelConf `json:"models"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, m := range f.Models {
		if err := m.check(getenv); err != nil {
			return nil, fmt.Errorf("%s: model %q: %w", path, name, err)
		}
	}
	return f.Models, nil
}

func (m *ModelConf) check(getenv func(string) string) error {
	if m.Model == "" {
		return fmt.Errorf("model is required")
	}
	if m.KeyEnv != "" {
		if m.key = getenv(m.KeyEnv); m.key == "" {
			return fmt.Errorf("environment variable %s is not set (put it in /etc/foreman.env)", m.KeyEnv)
		}
	}
	switch m.API {
	case "openai":
		if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
			return fmt.Errorf("url must be the API's base URL, e.g. http://127.0.0.1:8081/v1")
		}
		if m.Effort != "" || m.Fallbacks != nil {
			return fmt.Errorf("effort and fallbacks only apply to the anthropic api")
		}
		if m.MaxTokens == 0 {
			m.MaxTokens = 4096
		}
	case "anthropic":
		if m.key == "" {
			return fmt.Errorf("key_env is required for the anthropic api")
		}
		if m.Effort == "" {
			m.Effort = "medium"
		}
		if !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, m.Effort) {
			return fmt.Errorf("effort must be low, medium, high, xhigh or max")
		}
		if m.MaxTokens == 0 {
			m.MaxTokens = 16000
		}
	default:
		return fmt.Errorf(`api must be "openai" or "anthropic"`)
	}
	if m.MaxTokens < 1 || m.MaxTokens > 128000 {
		return fmt.Errorf("max_tokens must be 1-128000")
	}
	return nil
}

// eachDef calls f for every <name>.md in dir, with its front matter and body.
// A missing directory holds no definitions.
func eachDef(dir string, f func(name string, fm map[string]string, body string) error) error {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".md")
		rel := filepath.Join(filepath.Base(dir), filepath.Base(p))
		if !defNameRE.MatchString(name) {
			return fmt.Errorf("%s: the file name must be 1-40 characters of a-z 0-9 -", rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fm, body, err := frontMatter(string(b))
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if err := f(name, fm, body); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	return nil
}

// frontMatter splits "---\nkey: value\n---\nbody". A " #" starts a comment.
func frontMatter(s string) (map[string]string, string, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return nil, "", fmt.Errorf(`must start with a "---" line`)
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		head, ok = strings.CutSuffix(rest, "\n---")
		if !ok {
			return nil, "", fmt.Errorf(`front matter has no closing "---" line`)
		}
	}
	fm := map[string]string{}
	for i, line := range strings.Split(head, "\n") {
		if c := strings.Index(line, " #"); c >= 0 {
			line = line[:c]
		}
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, "", fmt.Errorf("line %d: want key: value", i+2)
		}
		k = strings.TrimSpace(k)
		if _, dup := fm[k]; dup {
			return nil, "", fmt.Errorf("%s is set twice", k)
		}
		fm[k] = strings.TrimSpace(v)
	}
	return fm, strings.TrimSpace(body), nil
}

func onlyKeys(fm map[string]string, allowed ...string) error {
	for k := range fm {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("unknown key %q (allowed: %s)", k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func parseAgent(name string, fm map[string]string, body string) (*Agent, error) {
	if err := onlyKeys(fm, "description", "profile", "model", "max_steps", "timeout"); err != nil {
		return nil, err
	}
	a := &Agent{Name: name, Description: fm["description"], Profile: fm["profile"], Model: fm["model"],
		MaxSteps: defaultMaxSteps, Timeout: defaultTimeout, Prompt: body}
	if !defNameRE.MatchString(a.Profile) {
		return nil, fmt.Errorf("profile is required: an mcp-hub profile name, or all")
	}
	if a.Model == "" {
		return nil, fmt.Errorf("model is required: a name from models.json")
	}
	if v := fm["max_steps"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxMaxSteps {
			return nil, fmt.Errorf("max_steps must be 1-%d", maxMaxSteps)
		}
		a.MaxSteps = n
	}
	if v := fm["timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > maxTimeout {
			return nil, fmt.Errorf("timeout must be a duration such as 5m, at most %s", maxTimeout)
		}
		a.Timeout = d
	}
	if a.Prompt == "" {
		return nil, fmt.Errorf("the prompt (after the front matter) is empty")
	}
	return a, nil
}

func parseTaskType(name string, fm map[string]string, body string) (*TaskType, error) {
	if err := onlyKeys(fm, "description", "on_event", "every", "agent", "group_by", "output", "actions"); err != nil {
		return nil, err
	}
	t := &TaskType{Name: name, Description: fm["description"], Agent: fm["agent"], Prompt: body,
		GroupBy: strings.Fields(fm["group_by"]), Output: strings.Fields(fm["output"])}
	if v := fm["on_event"]; v != "" {
		topic, types, ok := strings.Cut(v, " ")
		types = strings.TrimSpace(types)
		if !ok || !topicFiltRE.MatchString(topic) || !eventTypesRE.MatchString(types) {
			return nil, fmt.Errorf("on_event must be a relay topic filter and event types, e.g. duckpipe.* failed")
		}
		t.OnTopic, t.OnTypes = topic, strings.Split(types, ",")
	}
	if v := fm["every"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			return nil, fmt.Errorf("every must be a duration of at least 1m, e.g. 24h")
		}
		t.Every = d
	}
	if !defNameRE.MatchString(t.Agent) {
		return nil, fmt.Errorf("agent is required: the name of a file in agents/")
	}
	for _, k := range t.GroupBy {
		if !groupKeyRE.MatchString(k) {
			return nil, fmt.Errorf("group_by: %q must be topic, type, run_id or data.<key>", k)
		}
	}
	if len(t.Output) == 0 {
		t.Output = []string{"answer"}
	}
	for i, f := range t.Output {
		if !fieldNameRE.MatchString(f) {
			return nil, fmt.Errorf("output: %q must be a-z 0-9 _, starting with a letter", f)
		}
		if slices.Contains(t.Output[:i], f) {
			return nil, fmt.Errorf("output: %q is listed twice", f)
		}
	}
	for _, a := range strings.Split(fm["actions"], ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if !hubToolRE.MatchString(a) {
			return nil, fmt.Errorf("actions: %q must be a hub tool name, e.g. url-to-bucket__download", a)
		}
		if slices.Contains(t.Actions, a) {
			return nil, fmt.Errorf("actions: %q is listed twice", a)
		}
		t.Actions = append(t.Actions, a)
	}
	if t.Prompt == "" {
		return nil, fmt.Errorf("the instructions (after the front matter) are empty")
	}
	return t, nil
}
