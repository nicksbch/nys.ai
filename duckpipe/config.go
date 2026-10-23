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
	Lake struct {
		// DuckLake catalog, as ATTACH takes it: ducklake:sqlite:<file>,
		// ducklake:postgres:<conninfo> or ducklake:<file.ducklake>.
		Catalog string `yaml:"catalog"`
		// Where DuckLake writes table data, e.g. s3://lake/. Only used when the
		// catalog is created; afterwards the catalog remembers it.
		DataPath string `yaml:"data_path"`
		// Name the lake is attached under; tables are <name>.<schema>.<table>.
		Name string `yaml:"name"`
	} `yaml:"lake"`

	Poll duration `yaml:"poll"`

	Maintenance struct {
		// How often to merge small Parquet files. 0 disables maintenance.
		Every duration `yaml:"every"`
		// Drop snapshots older than this, and delete the files only they used.
		// Time travel reaches back this far. 0 keeps every snapshot.
		KeepSnapshots duration `yaml:"keep_snapshots"`
	} `yaml:"maintenance"`

	Pipes []*pipe `yaml:"pipes"`
}

type pipe struct {
	Name   string `yaml:"name"`
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
	// Glob on the file name, e.g. "*.csv". With a "/" it matches the whole key
	// below the prefix instead, e.g. "*/orders-*.csv". Empty matches everything.
	Pattern string `yaml:"pattern"`
	// csv, json or parquet. Empty picks by file extension.
	Format string `yaml:"format"`
	// Target table as [schema.]table. Default: main.<name>.
	Table string `yaml:"table"`
	// Extra reader options, as SQL, e.g. "delim = ';', all_varchar = true".
	// For JSON they go to read_json_objects.
	Options string `yaml:"options"`
	// Most files loaded in one transaction.
	Batch int `yaml:"batch"`

	schema, table string // parsed from Table
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
	identRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	pipeName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

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
	if c.Lake.Catalog == "" {
		return fmt.Errorf("lake.catalog is required, e.g. ducklake:sqlite:/var/lib/duckpipe/catalog.sqlite")
	}
	if !strings.HasPrefix(c.Lake.Catalog, "ducklake:") {
		c.Lake.Catalog = "ducklake:" + c.Lake.Catalog
	}
	if c.Lake.DataPath != "" && !strings.HasSuffix(c.Lake.DataPath, "/") {
		c.Lake.DataPath += "/"
	}
	if c.Lake.Name == "" {
		c.Lake.Name = "lake"
	}
	if !identRE.MatchString(c.Lake.Name) {
		return fmt.Errorf("lake.name %q: use letters, digits and _", c.Lake.Name)
	}
	if c.Poll.Duration == 0 {
		c.Poll.Duration = 30 * time.Second
	}
	if c.Poll.Duration < time.Second {
		return fmt.Errorf("poll %s is too short", c.Poll.Duration)
	}
	if len(c.Pipes) == 0 {
		return fmt.Errorf("no pipes configured")
	}

	seen := map[string]bool{}
	for i, p := range c.Pipes {
		if !pipeName.MatchString(p.Name) {
			return fmt.Errorf("pipes[%d]: name %q: use letters, digits, '.', '-' and '_'", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("pipe %s: name used twice", p.Name)
		}
		seen[p.Name] = true
		if p.Bucket == "" {
			return fmt.Errorf("pipe %s: bucket is required", p.Name)
		}
		if _, err := path.Match(p.Pattern, ""); err != nil {
			return fmt.Errorf("pipe %s: pattern %q: %w", p.Name, p.Pattern, err)
		}
		if p.Format != "" && reader[p.Format] == "" {
			return fmt.Errorf("pipe %s: format %q: use csv, json or parquet", p.Name, p.Format)
		}
		if p.Table == "" {
			p.Table = strings.NewReplacer(".", "_", "-", "_").Replace(p.Name)
		}
		p.schema, p.table = "main", p.Table
		if s, t, ok := strings.Cut(p.Table, "."); ok {
			p.schema, p.table = s, t
		}
		if !identRE.MatchString(p.schema) || !identRE.MatchString(p.table) || p.schema == historySchema {
			return fmt.Errorf("pipe %s: table %q: use [schema.]table with letters, digits and _ (schema %q is reserved)", p.Name, p.Table, historySchema)
		}
		if p.Batch <= 0 {
			p.Batch = 50
		}
	}
	return nil
}

// matches reports whether the pipe should load the object at key.
func (p *pipe) matches(key string) bool {
	rel := strings.TrimPrefix(key, p.Prefix)
	if rel == "" || strings.HasSuffix(rel, "/") { // folder markers
		return false
	}
	if p.Pattern == "" {
		return true
	}
	if !strings.Contains(p.Pattern, "/") {
		rel = path.Base(rel)
	}
	ok, _ := path.Match(p.Pattern, rel)
	return ok
}

// Table functions that read each format. JSON is read as one object per
// row, without guessing columns, so it lands in the record column as is.
var reader = map[string]string{
	"csv":     "read_csv",
	"json":    "read_json_objects",
	"parquet": "read_parquet",
}

// formatOf returns the pipe's format, or guesses it from the key's extension
// (compression suffixes are skipped; DuckDB decompresses those itself).
func (p *pipe) formatOf(key string) string {
	if p.Format != "" {
		return p.Format
	}
	name := strings.ToLower(path.Base(key))
	for _, z := range []string{".gz", ".zst"} {
		name = strings.TrimSuffix(name, z)
	}
	switch path.Ext(name) {
	case ".csv", ".tsv", ".txt":
		return "csv"
	case ".json", ".jsonl", ".ndjson":
		return "json"
	case ".parquet", ".pq":
		return "parquet"
	}
	return ""
}
