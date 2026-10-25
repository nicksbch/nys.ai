package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// job is what the service runs, and when: on a schedule, on relay events, or both.
type job struct {
	Name        string         `yaml:"name"`
	Select      string         `yaml:"select"`
	Exclude     string         `yaml:"exclude"`
	Schedule    string         `yaml:"schedule"` // 5-field cron, in the project's time zone
	On          []trigger      `yaml:"on"`
	Debounce    duration       `yaml:"debounce"`
	MaxWait     duration       `yaml:"max_wait"`
	FullRefresh bool           `yaml:"full_refresh"`
	Vars        map[string]any `yaml:"vars"`

	schedule cron.Schedule
}

// trigger matches relay events: topic in relay's filter syntax, type one or a
// comma list (empty: any), and data's top-level fields by equality.
type trigger struct {
	Topic string         `yaml:"topic" json:"topic"`
	Type  string         `yaml:"type" json:"type,omitempty"`
	Data  map[string]any `yaml:"data" json:"data,omitempty"`
}

type duration struct{ time.Duration }

func (d *duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

var (
	jobNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	topicRE   = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)
	typeRE    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// readJobs reads jobs.yml, if there is one, and checks every job's selection
// against the project.
func (p *project) readJobs() error {
	var f struct {
		Jobs []*job `yaml:"jobs"`
	}
	if err := readYAML(filepath.Join(p.dir, "jobs.yml"), &f, true); err != nil {
		return err
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	seen := map[string]bool{}
	for i, j := range f.Jobs {
		where := fmt.Sprintf("jobs.yml: job %s", j.Name)
		if !jobNameRE.MatchString(j.Name) {
			return fmt.Errorf("jobs.yml: jobs[%d]: name %q: use 1-64 letters, digits, '.', '-' and '_'", i, j.Name)
		}
		if seen[j.Name] {
			return fmt.Errorf("%s: name used twice", where)
		}
		seen[j.Name] = true
		if strings.TrimSpace(j.Select) == "" {
			return fmt.Errorf("%s: select is required", where)
		}
		if _, err := p.selectModels(j.Select, j.Exclude); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if j.Schedule == "" && len(j.On) == 0 {
			return fmt.Errorf("%s: needs a schedule, on: triggers, or both", where)
		}
		if j.Schedule != "" {
			s, err := parser.Parse(j.Schedule)
			if err != nil {
				return fmt.Errorf("%s: schedule %q: %w", where, j.Schedule, err)
			}
			j.schedule = s
		}
		for k, t := range j.On {
			if err := t.check(); err != nil {
				return fmt.Errorf("%s: on[%d]: %w", where, k, err)
			}
		}
		if j.Debounce.Duration == 0 {
			j.Debounce.Duration = time.Minute
		}
		if j.MaxWait.Duration == 0 {
			j.MaxWait.Duration = 10 * time.Minute
		}
		if j.Debounce.Duration < 0 || j.MaxWait.Duration < j.Debounce.Duration {
			return fmt.Errorf("%s: max_wait (%s) must be at least debounce (%s)", where, j.MaxWait.Duration, j.Debounce.Duration)
		}
	}
	p.jobs = f.Jobs
	return nil
}

func (t trigger) check() error {
	if t.Topic == "" {
		return fmt.Errorf("topic is required")
	}
	for _, pat := range splitList(t.Topic) {
		if pat != "*" && !topicRE.MatchString(strings.TrimSuffix(pat, ".*")) {
			return fmt.Errorf("topic %q: want a topic, prefix.* or *", pat)
		}
		if pat == "*" || strings.HasPrefix(pat, "lakeshape.") || pat == "lakeshape.*" {
			return fmt.Errorf("topic %q: lakeshape's own events never trigger jobs; name the topics to watch", pat)
		}
	}
	for _, typ := range splitList(t.Type) {
		if !typeRE.MatchString(typ) {
			return fmt.Errorf("type %q: use 1-64 letters, digits, '.', '-' and '_'", typ)
		}
	}
	for k, v := range t.Data {
		switch v.(type) {
		case string, int, float64, bool:
		default:
			return fmt.Errorf("data.%s: compare with a string, number or boolean", k)
		}
	}
	return nil
}

// matches reports whether event e fires the trigger.
func (t trigger) matches(e relayEvent) bool {
	if !matchTopics(t.Topic, e.Topic) {
		return false
	}
	if types := splitList(t.Type); len(types) > 0 && !slices.Contains(types, e.Type) {
		return false
	}
	for k, want := range t.Data {
		got, ok := e.Data[k]
		if !ok || !sameJSON(got, want) {
			return false
		}
	}
	return true
}

// matchTopics matches a topic against a comma list of relay topic filters.
func matchTopics(pats, topic string) bool {
	for _, pat := range splitList(pats) {
		switch {
		case pat == "*":
			return true
		case strings.HasSuffix(pat, ".*") && strings.HasPrefix(topic, pat[:len(pat)-1]):
			return true
		case pat == topic:
			return true
		}
	}
	return false
}

// sameJSON compares two values as JSON, so 5 from YAML equals 5 from JSON.
func sameJSON(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
