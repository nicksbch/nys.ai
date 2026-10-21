package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// historySchema holds lakeshape's own tables (run_history). No model may use it.
const historySchema = "lakeshape"

// devPrefix starts every dev schema: dev_<user>__<schema>. Prod models may not
// use schemas starting with it, so dev and prod can never collide.
const devPrefix = "dev_"

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// project is a loaded and parsed project, for one target.
type project struct {
	dir string

	Name string `yaml:"name"`
	Lake struct {
		// DuckLake catalog, as ATTACH takes it, e.g. ducklake:postgres:dbname=ducklake.
		Catalog string `yaml:"catalog"`
		// Where DuckLake writes table data, e.g. s3://lake/.
		DataPath string `yaml:"data_path"`
	} `yaml:"lake"`
	// Default schema for models.
	Schema   string         `yaml:"schema"`
	Timezone string         `yaml:"timezone"`
	Threads  int            `yaml:"threads"`
	Vars     map[string]any `yaml:"vars"`
	// Model config by folder under models/, e.g. "staging" or "marts/finance".
	Folders map[string]modelConfig `yaml:"models"`

	location *time.Location
	target   target
	sources  map[string]map[string]string // source name -> table -> schema
	macros   []macroFile
	models   map[string]*model
	order    []*model // dependencies first
	r        *renderer
	// Singular tests' templates, by name, and every test.
	singulars map[string]*model
	tests     []*test
	jobs      []*job
}

// modelConfig is what config(), project.yaml's folders and the defaults set.
// Empty fields are unset.
type modelConfig struct {
	Materialized        string     `yaml:"materialized"`
	Schema              string     `yaml:"schema"`
	UniqueKey           stringList `yaml:"unique_key"`
	IncrementalStrategy string     `yaml:"incremental_strategy"`
	Tags                stringList `yaml:"tags"`
}

// stringList is a YAML string or list of strings.
type stringList []string

func (l *stringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = stringList{n.Value}
		return nil
	}
	var s []string
	if err := n.Decode(&s); err != nil {
		return err
	}
	*l = s
	return nil
}

// over returns c with every field o sets replacing c's. Tags add up.
func (c modelConfig) over(o modelConfig) modelConfig {
	if o.Materialized != "" {
		c.Materialized = o.Materialized
	}
	if o.Schema != "" {
		c.Schema = o.Schema
	}
	if o.UniqueKey != nil {
		c.UniqueKey = o.UniqueKey
	}
	if o.IncrementalStrategy != "" {
		c.IncrementalStrategy = o.IncrementalStrategy
	}
	for _, t := range o.Tags {
		if !slices.Contains(c.Tags, t) {
			c.Tags = append(c.Tags, t)
		}
	}
	return c
}

type model struct {
	name string
	path string // relative to the project, e.g. models/marts/orders.sql
	raw  string
	cfg  modelConfig // resolved
	refs []string    // models it depends on, in call order
	srcs []string    // source tables it reads, as source.table
	// Models a relationships test checks it against: built before it, but
	// not refs.
	testDeps []string
	// A singular test's template, not a model.
	test bool
	// Tests that check only this model; they run in its transaction.
	tests []*test
}

// deps returns the models m is built after.
func (m *model) deps() []string { return union(slices.Clone(m.refs), m.testDeps) }

type macroFile struct {
	path  string // e.g. macros/money.sql
	names []string
}

type target struct {
	name string // prod or dev
	user string // dev only
}

// schema returns the schema a model with schema s is built in.
func (t target) schema(s string) string {
	if t.name == "dev" {
		return devPrefix + t.user + "__" + s
	}
	return s
}

// devUser turns a user name into the part of a dev schema that names it.
func devUser(s string) (string, error) {
	u := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, strings.ToLower(s))
	if strings.Trim(u, "_") == "" {
		return "", fmt.Errorf("dev user %q: needs at least one letter or digit (set -dev-user)", s)
	}
	return u, nil
}

// checkWrite refuses, in the dev target, a write to a schema that is not the
// user's own. It guards every statement lakeshape writes with.
func (t target) checkWrite(schema string) error {
	if own := devPrefix + t.user + "__"; t.name == "dev" && !strings.HasPrefix(schema, own) {
		return fmt.Errorf("dev target: refusing to write to schema %s; dev only writes %s* schemas", schema, own)
	}
	return nil
}

// relation returns where m is built: schema.table.
func (p *project) relation(m *model) string {
	return p.target.schema(m.cfg.Schema) + "." + m.name
}

// loadProject reads the project in dir and renders every model once for t,
// to find its config and dependencies. vars override project.yaml's.
func loadProject(dir string, t target, vars map[string]any) (*project, error) {
	p := &project{dir: dir, target: t}
	if err := readYAML(filepath.Join(dir, "project.yaml"), p, false); err != nil {
		return nil, err
	}
	if err := p.check(); err != nil {
		return nil, fmt.Errorf("project.yaml: %w", err)
	}
	if p.Vars == nil {
		p.Vars = map[string]any{}
	}
	for k, v := range vars {
		p.Vars[k] = v
	}
	if err := p.readSources(); err != nil {
		return nil, err
	}
	if err := p.readFiles(); err != nil {
		return nil, err
	}
	p.r = newRenderer(p)
	if err := p.r.findMacros(); err != nil {
		return nil, err
	}
	if err := p.parseModels(p.r); err != nil {
		return nil, err
	}
	if err := p.readTests(); err != nil {
		return nil, err
	}
	order, err := sortModels(p.models)
	if err != nil {
		return nil, err
	}
	p.order = order
	for _, t := range p.tests {
		if t.own() {
			m := p.models[t.models[0]]
			m.tests = append(m.tests, t)
		}
	}
	if err := p.readJobs(); err != nil {
		return nil, err
	}
	return p, nil
}

func readYAML(file string, v any, optional bool) error {
	b, err := os.ReadFile(file)
	if optional && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && err.Error() != "EOF" {
		return fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	return nil
}

func (p *project) check() error {
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	if p.Lake.Catalog != "" && !strings.HasPrefix(p.Lake.Catalog, "ducklake:") {
		p.Lake.Catalog = "ducklake:" + p.Lake.Catalog
	}
	if p.Lake.DataPath != "" && !strings.HasSuffix(p.Lake.DataPath, "/") {
		p.Lake.DataPath += "/"
	}
	if p.Schema == "" {
		p.Schema = "main"
	}
	if err := checkSchema(p.Schema); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	if p.Timezone == "" {
		p.Timezone = "UTC"
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	p.location = loc
	if p.Threads == 0 {
		p.Threads = 4
	}
	if p.Threads < 1 {
		return fmt.Errorf("threads must be 1 or more")
	}
	for folder, c := range p.Folders {
		clean := filepath.ToSlash(filepath.Clean(folder))
		if clean != folder || strings.HasPrefix(folder, "/") || strings.HasPrefix(folder, "..") || folder == "." {
			return fmt.Errorf("models: folder %q: use a path below models/, like staging or marts/finance", folder)
		}
		if st, err := os.Stat(filepath.Join(p.dir, "models", folder)); err != nil || !st.IsDir() {
			return fmt.Errorf("models: folder %q: there is no models/%s folder", folder, folder)
		}
		if err := checkConfig(c); err != nil {
			return fmt.Errorf("models: %s: %w", folder, err)
		}
	}
	return nil
}

// checkSchema rejects schema names models may not be built in.
func checkSchema(s string) error {
	switch {
	case !identRE.MatchString(s):
		return fmt.Errorf("%q: use letters, digits and _", s)
	case strings.EqualFold(s, historySchema):
		return fmt.Errorf("%q is reserved for lakeshape's own tables", s)
	case strings.HasPrefix(strings.ToLower(s), devPrefix):
		return fmt.Errorf("%q: schemas starting with %s are reserved for dev targets", s, devPrefix)
	}
	return nil
}

// checkConfig checks the values a config sets, but not whether they fit together.
func checkConfig(c modelConfig) error {
	switch c.Materialized {
	case "", "view", "table", "incremental":
	default:
		return fmt.Errorf("materialized %q: use view, table or incremental", c.Materialized)
	}
	if c.Schema != "" {
		if err := checkSchema(c.Schema); err != nil {
			return fmt.Errorf("schema %w", err)
		}
	}
	switch c.IncrementalStrategy {
	case "", "append", "delete+insert":
	default:
		return fmt.Errorf("incremental_strategy %q: use append or delete+insert", c.IncrementalStrategy)
	}
	for _, k := range c.UniqueKey {
		if !identRE.MatchString(k) {
			return fmt.Errorf("unique_key %q: use a column name", k)
		}
	}
	for _, t := range c.Tags {
		if !identRE.MatchString(t) {
			return fmt.Errorf("tag %q: use letters, digits and _", t)
		}
	}
	return nil
}

func (p *project) readSources() error {
	var f struct {
		Sources []struct {
			Name   string   `yaml:"name"`
			Schema string   `yaml:"schema"`
			Tables []string `yaml:"tables"`
		} `yaml:"sources"`
	}
	if err := readYAML(filepath.Join(p.dir, "sources.yml"), &f, true); err != nil {
		return err
	}
	p.sources = map[string]map[string]string{}
	for i, s := range f.Sources {
		if !identRE.MatchString(s.Name) {
			return fmt.Errorf("sources.yml: sources[%d]: name %q: use letters, digits and _", i, s.Name)
		}
		if p.sources[s.Name] != nil {
			return fmt.Errorf("sources.yml: source %s: declared twice", s.Name)
		}
		if s.Schema == "" {
			s.Schema = s.Name
		}
		if !identRE.MatchString(s.Schema) {
			return fmt.Errorf("sources.yml: source %s: schema %q: use letters, digits and _", s.Name, s.Schema)
		}
		if len(s.Tables) == 0 {
			return fmt.Errorf("sources.yml: source %s: no tables", s.Name)
		}
		tables := map[string]string{}
		for _, t := range s.Tables {
			if !identRE.MatchString(t) {
				return fmt.Errorf("sources.yml: source %s: table %q: use letters, digits and _", s.Name, t)
			}
			if tables[t] != "" {
				return fmt.Errorf("sources.yml: source %s: table %s listed twice", s.Name, t)
			}
			tables[t] = s.Schema
		}
		p.sources[s.Name] = tables
	}
	return nil
}

// readFiles finds the models (models/**/*.sql) and macro files (macros/*.sql).
// Files and folders whose names start with "." are skipped.
func (p *project) readFiles() error {
	p.models = map[string]*model{}
	root := filepath.Join(p.dir, "models")
	err := filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && file != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || filepath.Ext(file) != ".sql" {
			return nil
		}
		rel, _ := filepath.Rel(p.dir, file)
		rel = filepath.ToSlash(rel)
		name := strings.TrimSuffix(d.Name(), ".sql")
		if !identRE.MatchString(name) {
			return fmt.Errorf("%s: model names (file names) use letters, digits and _", rel)
		}
		if m := p.models[name]; m != nil {
			return fmt.Errorf("model %s is defined twice: %s and %s", name, m.path, rel)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		p.models[name] = &model{name: name, path: rel, raw: string(b)}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no models/ folder in %s", p.dir)
	}
	if err != nil {
		return err
	}

	files, err := filepath.Glob(filepath.Join(p.dir, "macros", "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), ".") {
			continue
		}
		p.macros = append(p.macros, macroFile{path: "macros/" + filepath.Base(f)})
	}

	p.singulars = map[string]*model{}
	files, err = filepath.Glob(filepath.Join(p.dir, "tests", "*.sql"))
	if err != nil {
		return err
	}
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, ".") {
			continue
		}
		rel := "tests/" + base
		name := strings.TrimSuffix(base, ".sql")
		if !identRE.MatchString(name) {
			return fmt.Errorf("%s: test names (file names) use letters, digits and _", rel)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		p.singulars[name] = &model{name: name, path: rel, raw: string(b), test: true}
	}
	return nil
}

// folderConfig returns the project defaults with every folder config that
// applies to the model at path laid over them, shallowest first.
func (p *project) folderConfig(path string) modelConfig {
	c := modelConfig{Materialized: "view", Schema: p.Schema}
	dir := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(path)), "models")
	var parts []string
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		parts = append(parts, part)
		if f, ok := p.Folders[strings.Join(parts, "/")]; ok {
			c = c.over(f)
		}
	}
	return c
}

// parseModels renders every model to find its config and dependencies.
// Incremental models are rendered both as a first build and as an incremental
// one, and depend on what either refers to.
func (p *project) parseModels(r *renderer) error {
	var errs []string
	for _, name := range sortedKeys(p.models) {
		m := p.models[name]
		if err := p.parseModel(r, m); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

func (p *project) parseModel(r *renderer, m *model) error {
	res, err := r.render(m, parsePhase, false)
	if err != nil {
		return err
	}
	m.cfg = p.resolveConfig(m, res.cfg)
	m.refs, m.srcs = res.refs, res.srcs
	if m.cfg.Materialized == "incremental" {
		inc, err := r.render(m, parsePhase, true)
		if err != nil {
			return err
		}
		if !sameConfig(p.resolveConfig(m, inc.cfg), m.cfg) {
			return fmt.Errorf("%s: config() must be the same whether or not is_incremental() is true", m.path)
		}
		m.refs = union(m.refs, inc.refs)
		m.srcs = union(m.srcs, inc.srcs)
	}
	if m.cfg.Materialized == "incremental" && m.cfg.IncrementalStrategy == "delete+insert" && len(m.cfg.UniqueKey) == 0 {
		return fmt.Errorf("%s: incremental_strategy delete+insert needs a unique_key", m.path)
	}
	// Checked in every target: in dev it would only fail once deployed.
	for _, src := range sortedKeys(p.sources) {
		if tables := p.sources[src]; tables[m.name] == m.cfg.Schema {
			return fmt.Errorf("%s: the model would be built in %s.%s, which is source('%s', '%s')", m.path, m.cfg.Schema, m.name, src, m.name)
		}
	}
	return nil
}

// resolveConfig lays what a model's config() calls set over its folder config,
// and fills in the incremental strategy.
func (p *project) resolveConfig(m *model, set modelConfig) modelConfig {
	c := p.folderConfig(m.path).over(set)
	if c.IncrementalStrategy == "" {
		c.IncrementalStrategy = "append"
		if len(c.UniqueKey) > 0 {
			c.IncrementalStrategy = "delete+insert"
		}
	}
	return c
}

// compile returns the SQL of model m. incremental is what is_incremental()
// returns. The render must call config(), ref() and source() as the parse
// phase did.
func (p *project) compile(m *model, incremental bool) (string, error) {
	res, err := p.r.render(m, compilePhase, incremental)
	if err != nil {
		return "", err
	}
	changed := !m.test && !sameConfig(p.resolveConfig(m, res.cfg), m.cfg)
	for _, ref := range res.refs {
		changed = changed || !slices.Contains(m.refs, ref)
	}
	for _, src := range res.srcs {
		changed = changed || !slices.Contains(m.srcs, src)
	}
	if changed {
		return "", fmt.Errorf("%s: config(), ref() or source() calls depend on the value of ref() or this; they must not", m.path)
	}
	return res.sql, nil
}

func readFile(dir, rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, rel))
	return string(b), err
}

func sameConfig(a, b modelConfig) bool {
	return a.Materialized == b.Materialized && a.Schema == b.Schema &&
		a.IncrementalStrategy == b.IncrementalStrategy &&
		slices.Equal(a.UniqueKey, b.UniqueKey) && slices.Equal(a.Tags, b.Tags)
}

// union returns a with the elements of b it lacks appended.
func union(a, b []string) []string {
	for _, s := range b {
		if !slices.Contains(a, s) {
			a = append(a, s)
		}
	}
	return a
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
