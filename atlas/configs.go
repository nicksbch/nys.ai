package main

// Reading the services' config files. Each file is parsed for the few fields
// that make links, and everything else is ignored, so a new field in a
// service's config never breaks atlas. A file that doesn't parse is kept with
// its error, and the rest still loads.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// paths says where each service's config is.
type paths struct {
	Preprocessor string // preprocessor.yaml
	Duckpipe     string // duckpipe.yaml
	Lakeshape    string // the project directory
	Semantic     string // models.yml
	MCPHub       string // servers.json
	Foreman      string // the config directory: agents/, tasks/
	Analyst      string // the config directory: models.json
}

// serverPaths are the files the deploy scripts install.
var serverPaths = paths{
	Preprocessor: "/etc/preprocessor.yaml",
	Duckpipe:     "/etc/duckpipe.yaml",
	Lakeshape:    "/etc/lakeshape/project",
	Semantic:     "/opt/semantic/app/models.yml",
	MCPHub:       "/etc/mcp-hub/servers.json",
	Foreman:      "/etc/foreman",
	Analyst:      "/etc/analyst",
}

// repoPaths are the same files in a checkout of this repo.
func repoPaths(root string) paths {
	j := func(p ...string) string { return filepath.Join(append([]string{root}, p...)...) }
	return paths{
		Preprocessor: j("preprocessor", "preprocessor.yaml"),
		Duckpipe:     j("duckpipe", "duckpipe.yaml"),
		Lakeshape:    j("analytics"),
		Semantic:     j("semantic", "models.yml"),
		MCPHub:       j("mcp-hub", "servers.json"),
		Foreman:      j("foreman", "config"),
		Analyst:      j("analyst", "config"),
	}
}

// file is one config file as read, shown whole on the Configs tab.
type file struct {
	Service string    `json:"service"`
	Path    string    `json:"path"`
	ModTime time.Time `json:"mod_time"`
	Text    string    `json:"text"`
	Error   string    `json:"error,omitempty"`

	lines []string
}

// loc is where something is defined: a file, a line, and the lines of its block.
type loc struct {
	File  string `json:"file"`
	Line  int    `json:"line"`
	Block string `json:"block"`
}

type (
	preJob struct {
		Name, Bucket, Prefix, Pattern, To string
		at                                loc
	}
	pipe struct {
		Name, Bucket, Prefix, Pattern, Table string
		at                                   loc
	}
	trigger struct {
		Topic string         `yaml:"topic"`
		Type  string         `yaml:"type"`
		Data  map[string]any `yaml:"data"`
	}
	lsJob struct {
		Name     string    `yaml:"name"`
		Select   string    `yaml:"select"`
		Exclude  string    `yaml:"exclude"`
		Schedule string    `yaml:"schedule"`
		On       []trigger `yaml:"on"`
		at       loc
	}
	source struct {
		Name, Schema, Table string // source name, and the table it reads
		at                  loc
	}
	mart struct {
		Name string
		at   loc
	}
	semModel struct {
		Name, Table string
		at          loc
	}
	hubServer struct {
		Name, URL string
		at        loc
	}
	hubProfile struct {
		Name    string
		Servers []string
		at      loc
	}
	agent struct {
		Name, Profile, Model string
		at                   loc
	}
	task struct {
		Name, OnEvent, Every, Agent string
		at                          loc
	}
)

// configs is everything read, plus the files and what went wrong.
type configs struct {
	PreJobs  []preJob
	Pipes    []pipe
	LsJobs   []lsJob
	Sources  []source
	Marts    []mart
	Semantic []semModel
	Servers  []hubServer
	Profiles []hubProfile
	Agents   []agent
	Tasks    []task
	Analyst  *loc // nil if analyst's config isn't there

	Files    []*file
	Problems []problem
}

type problem struct {
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text"`
}

func readConfigs(p paths) *configs {
	c := &configs{}
	c.readPreprocessor(p.Preprocessor)
	c.readDuckpipe(p.Duckpipe)
	c.readLakeshape(p.Lakeshape)
	c.readSemantic(p.Semantic)
	c.readHub(p.MCPHub)
	c.readForeman(p.Foreman)
	c.readAnalyst(p.Analyst)
	return c
}

// read loads a file, records it, and returns it, or nil (with a problem) if
// it can't be read. A missing optional file returns nil quietly.
func (c *configs) read(service, path string, optional bool) *file {
	b, err := os.ReadFile(path)
	if err != nil {
		if optional && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		c.Files = append(c.Files, &file{Service: service, Path: path, Error: err.Error()})
		c.Problems = append(c.Problems, problem{File: path, Text: "can't read it: " + err.Error()})
		return nil
	}
	f := &file{Service: service, Path: path, Text: string(b)}
	if st, err := os.Stat(path); err == nil {
		f.ModTime = st.ModTime()
	}
	f.lines = strings.Split(strings.ReplaceAll(f.Text, "\r\n", "\n"), "\n")
	c.Files = append(c.Files, f)
	return f
}

func (c *configs) fail(f *file, line int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	f.Error = msg
	c.Problems = append(c.Problems, problem{File: f.Path, Line: line, Text: msg})
}

// --- YAML --------------------------------------------------------------------

func (c *configs) yamlRoot(f *file) *yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(f.Text), &doc); err != nil {
		c.fail(f, 0, "doesn't parse: %v", err)
		return nil
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		c.fail(f, 0, "doesn't parse: not a YAML mapping")
		return nil
	}
	return doc.Content[0]
}

// key returns the value of key in a mapping, and the line of the key.
func key(m *yaml.Node, k string) (*yaml.Node, int) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, 0
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			return m.Content[i+1], m.Content[i].Line
		}
	}
	return nil, 0
}

// items returns a sequence's items with the line each one's block ends on:
// the line before the next item, or the next key of the enclosing mapping.
func items(f *file, root *yaml.Node, k string) ([]*yaml.Node, []int) {
	seq, _ := key(root, k)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil, nil
	}
	end := len(f.lines)
	for i := 0; i+1 < len(root.Content); i += 2 {
		if l := root.Content[i].Line; l > seq.Line && l-1 < end {
			end = l - 1
		}
	}
	ends := make([]int, len(seq.Content))
	for i := range seq.Content {
		ends[i] = end
		if i+1 < len(seq.Content) {
			ends[i] = seq.Content[i+1].Line - 1
		}
	}
	return seq.Content, ends
}

// at makes a loc for lines from..to (1-based), taking in the comment lines
// just above and dropping comment and blank lines at the end, which belong
// to whatever comes next.
func (f *file) at(from, to int) loc {
	if to > len(f.lines) {
		to = len(f.lines)
	}
	for from > 1 && isComment(f.lines[from-2]) {
		from--
	}
	for to > from && (isComment(f.lines[to-1]) || strings.TrimSpace(f.lines[to-1]) == "") {
		to--
	}
	if from < 1 || from > to {
		return loc{File: f.Path, Line: from}
	}
	return loc{File: f.Path, Line: from, Block: strings.Join(f.lines[from-1:to], "\n")}
}

func isComment(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }

// line returns the 1-based line holding s, or 0.
func (f *file) line(s string) int {
	for i, l := range f.lines {
		if strings.Contains(l, s) {
			return i + 1
		}
	}
	return 0
}

// --- preprocessor and duckpipe -----------------------------------------------

func (c *configs) readPreprocessor(path string) {
	f := c.read("preprocessor", path, false)
	root := f.root(c)
	its, ends := items(f, root, "jobs")
	for i, n := range its {
		var j preJob
		if err := n.Decode(&struct {
			Name    *string `yaml:"name"`
			Bucket  *string `yaml:"bucket"`
			Prefix  *string `yaml:"prefix"`
			Pattern *string `yaml:"pattern"`
			To      *string `yaml:"to"`
		}{&j.Name, &j.Bucket, &j.Prefix, &j.Pattern, &j.To}); err != nil {
			c.fail(f, n.Line, "jobs: %v", err)
			continue
		}
		if j.Pattern == "" {
			j.Pattern = "*.zip" // preprocessor's default
		}
		j.at = f.at(n.Line, ends[i])
		c.PreJobs = append(c.PreJobs, j)
	}
}

func (c *configs) readDuckpipe(path string) {
	f := c.read("duckpipe", path, false)
	root := f.root(c)
	its, ends := items(f, root, "pipes")
	for i, n := range its {
		var p pipe
		if err := n.Decode(&struct {
			Name    *string `yaml:"name"`
			Bucket  *string `yaml:"bucket"`
			Prefix  *string `yaml:"prefix"`
			Pattern *string `yaml:"pattern"`
			Table   *string `yaml:"table"`
		}{&p.Name, &p.Bucket, &p.Prefix, &p.Pattern, &p.Table}); err != nil {
			c.fail(f, n.Line, "pipes: %v", err)
			continue
		}
		if p.Table == "" {
			p.Table = "main." + p.Name // duckpipe's default
		} else if !strings.Contains(p.Table, ".") {
			p.Table = "main." + p.Table
		}
		p.at = f.at(n.Line, ends[i])
		c.Pipes = append(c.Pipes, p)
	}
}

// root parses a YAML file read by c.read; nil-safe.
func (f *file) root(c *configs) *yaml.Node {
	if f == nil {
		return nil
	}
	return c.yamlRoot(f)
}

// --- lakeshape ---------------------------------------------------------------

func (c *configs) readLakeshape(dir string) {
	if f := c.read("lakeshape", filepath.Join(dir, "jobs.yml"), false); f != nil {
		root := f.root(c)
		its, ends := items(f, root, "jobs")
		for i, n := range its {
			var j lsJob
			if err := n.Decode(&j); err != nil {
				c.fail(f, n.Line, "jobs: %v", err)
				continue
			}
			j.at = f.at(n.Line, ends[i])
			c.LsJobs = append(c.LsJobs, j)
		}
	}
	if f := c.read("lakeshape", filepath.Join(dir, "sources.yml"), false); f != nil {
		root := f.root(c)
		its, _ := items(f, root, "sources")
		for _, n := range its {
			var s struct {
				Name   string   `yaml:"name"`
				Schema string   `yaml:"schema"`
				Tables []string `yaml:"tables"`
			}
			if err := n.Decode(&s); err != nil {
				c.fail(f, n.Line, "sources: %v", err)
				continue
			}
			if s.Schema == "" {
				s.Schema = s.Name
			}
			tables, _ := key(n, "tables")
			for i, t := range s.Tables {
				line := n.Line
				if tables != nil && i < len(tables.Content) {
					line = tables.Content[i].Line
				}
				c.Sources = append(c.Sources, source{Name: s.Name, Schema: s.Schema, Table: t, at: f.at(line, line)})
			}
		}
	}
	c.read("lakeshape", filepath.Join(dir, "project.yaml"), false)
	sqls, _ := filepath.Glob(filepath.Join(dir, "models", "marts", "*.sql"))
	sort.Strings(sqls)
	for _, p := range sqls {
		f := c.read("lakeshape", p, false)
		if f == nil {
			continue
		}
		c.Marts = append(c.Marts, mart{Name: strings.TrimSuffix(filepath.Base(p), ".sql"), at: f.at(1, len(f.lines))})
	}
}

// --- semantic ----------------------------------------------------------------

func (c *configs) readSemantic(path string) {
	f := c.read("semantic", path, false)
	root := f.root(c)
	if root == nil {
		return
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		end := len(f.lines)
		if i+2 < len(root.Content) {
			end = root.Content[i+2].Line - 1
		}
		var m struct {
			Table string `yaml:"table"`
		}
		if err := v.Decode(&m); err != nil {
			c.fail(f, k.Line, "%s: %v", k.Value, err)
			continue
		}
		c.Semantic = append(c.Semantic, semModel{Name: k.Value, Table: m.Table, at: f.at(k.Line, end)})
	}
}

// --- mcp-hub -----------------------------------------------------------------

func (c *configs) readHub(path string) {
	f := c.read("mcp-hub", path, false)
	if f == nil {
		return
	}
	var h struct {
		Servers []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"servers"`
		Profiles map[string]struct {
			Servers []string `json:"servers"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(f.Text), &h); err != nil {
		c.fail(f, 0, "doesn't parse: %v", err)
		return
	}
	for _, s := range h.Servers {
		l := f.line(fmt.Sprintf("%q", s.Name))
		c.Servers = append(c.Servers, hubServer{Name: s.Name, URL: s.URL, at: f.at(l, l)})
	}
	names := make([]string, 0, len(h.Profiles))
	for n := range h.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := f.line(fmt.Sprintf("%q:", n))
		c.Profiles = append(c.Profiles, hubProfile{Name: n, Servers: h.Profiles[n].Servers, at: f.at(l, l)})
	}
}

// --- foreman and analyst -----------------------------------------------------

func (c *configs) readForeman(dir string) {
	for _, kind := range []string{"agents", "tasks"} {
		mds, _ := filepath.Glob(filepath.Join(dir, kind, "*.md"))
		sort.Strings(mds)
		for _, p := range mds {
			f := c.read("foreman", p, false)
			if f == nil {
				continue
			}
			fm, end, err := frontMatter(f.lines)
			if err != nil {
				c.fail(f, 1, "%v", err)
				continue
			}
			name := strings.TrimSuffix(filepath.Base(p), ".md")
			at := f.at(1, end)
			if kind == "agents" {
				c.Agents = append(c.Agents, agent{Name: name, Profile: fm["profile"], Model: fm["model"], at: at})
			} else {
				c.Tasks = append(c.Tasks, task{Name: name, OnEvent: fm["on_event"], Every: fm["every"], Agent: fm["agent"], at: at})
			}
		}
	}
}

// frontMatter reads foreman's "---\nkey: value\n---" header, where " #"
// starts a comment. It returns the line of the closing "---".
func frontMatter(lines []string) (map[string]string, int, error) {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, 0, errors.New(`must start with a "---" line`)
	}
	fm := map[string]string{}
	for i := 1; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "---" {
			return fm, i + 1, nil
		}
		if c := strings.Index(l, " #"); c >= 0 {
			l = l[:c]
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return nil, 0, errors.New(`front matter has no closing "---" line`)
}

func (c *configs) readAnalyst(dir string) {
	f := c.read("analyst", filepath.Join(dir, "models.json"), true)
	if f == nil {
		return
	}
	at := f.at(1, len(f.lines))
	c.Analyst = &at
}

// --- helpers used when linking -----------------------------------------------

// tableID normalises a table name to schema.table: lake.uk.x and uk.x are
// the same table.
func tableID(s string) string {
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, ".")
}
