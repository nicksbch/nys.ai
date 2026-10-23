package main

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type config struct {
	Poll duration `yaml:"poll"`
	Jobs []*job   `yaml:"jobs"`
}

// A job unzips every archive below bucket/prefix into bucket/to.
type job struct {
	Name   string `yaml:"name"`
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
	// Glob on the file name, e.g. "*.zip". With a "/" it matches the whole key
	// below the prefix instead, e.g. "*/crime-*.zip". Default: "*.zip".
	Pattern string `yaml:"pattern"`
	// Prefix, in the same bucket, the entries are written to under their own
	// path in the archive. Keys below it are never read as archives.
	To string `yaml:"to"`
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

var jobName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func loadConfig(file string) (*config, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var c config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &c, nil
}

func (c *config) check() error {
	if c.Poll.Duration == 0 {
		c.Poll.Duration = time.Minute
	}
	if c.Poll.Duration < time.Second {
		return fmt.Errorf("poll %s is too short", c.Poll.Duration)
	}
	if len(c.Jobs) == 0 {
		return fmt.Errorf("no jobs configured")
	}

	seen := map[string]bool{}
	for i, j := range c.Jobs {
		if !jobName.MatchString(j.Name) {
			return fmt.Errorf("jobs[%d]: name %q: use letters, digits, '.', '-' and '_'", i, j.Name)
		}
		if seen[j.Name] {
			return fmt.Errorf("job %s: name used twice", j.Name)
		}
		seen[j.Name] = true
		if j.Bucket == "" {
			return fmt.Errorf("job %s: bucket is required", j.Name)
		}
		if j.Pattern == "" {
			j.Pattern = "*.zip"
		}
		if _, err := path.Match(j.Pattern, ""); err != nil {
			return fmt.Errorf("job %s: pattern %q: %w", j.Name, j.Pattern, err)
		}
		if j.To == "" {
			return fmt.Errorf("job %s: to is required, e.g. %sextracted/", j.Name, j.Prefix)
		}
		if !strings.HasSuffix(j.To, "/") {
			j.To += "/"
		}
		if strings.HasPrefix(j.To, "/") || path.Clean(j.To) != strings.TrimSuffix(j.To, "/") {
			return fmt.Errorf("job %s: to %q: want a plain prefix like uk/crime/extracted/", j.Name, j.To)
		}
		// Archives written to "to" would be unzipped by the job that wrote
		// them, and their entries written again inside each other.
		if j.Prefix != "" && strings.HasPrefix(j.Prefix, j.To) {
			return fmt.Errorf("job %s: prefix %q is inside to %q", j.Name, j.Prefix, j.To)
		}
	}
	return nil
}

// matches reports whether the job should unzip the object at key.
func (j *job) matches(key string) bool {
	if !strings.HasPrefix(key, j.Prefix) || strings.HasPrefix(key, j.To) {
		return false
	}
	rel := strings.TrimPrefix(key, j.Prefix)
	if rel == "" || strings.HasSuffix(rel, "/") { // folder markers
		return false
	}
	if !strings.Contains(j.Pattern, "/") {
		rel = path.Base(rel)
	}
	ok, _ := path.Match(j.Pattern, rel)
	return ok
}

// entryKey returns where an archive entry is written, or an error for a name
// that would land outside the job's "to" prefix ("../x", "/x", "a/../../x").
func (j *job) entryKey(name string) (string, error) {
	if name == "" || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") ||
		path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("entry %q: unsafe path in archive", name)
	}
	return j.To + name, nil
}
