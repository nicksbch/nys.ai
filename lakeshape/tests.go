package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// test is a query that returns bad rows. It passes when there are none.
type test struct {
	id       string // e.g. unique_orders_order_id, or a singular test's file name
	path     string // file it is declared in
	severity string // error or warn
	models   []string
	sql      string

	singular *model // the template of a singular test
}

// own reports whether t runs inside its model's transaction: it checks one
// model. Tests on several models run once all of them have committed.
func (t *test) own() bool { return len(t.models) == 1 }

type testFile struct {
	Models []struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Columns     []struct {
			Name        string      `yaml:"name"`
			Description string      `yaml:"description"`
			Tests       []yaml.Node `yaml:"tests"`
		} `yaml:"columns"`
	} `yaml:"models"`
}

// readTests reads the column tests in models/**/*.yml and the singular tests
// in tests/*.sql. Relations are those of p's target.
func (p *project) readTests() error {
	root := filepath.Join(p.dir, "models")
	var files []string
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
		if ext := filepath.Ext(file); !d.IsDir() && (ext == ".yml" || ext == ".yaml") {
			files = append(files, file)
		}
		return nil
	})
	if err != nil {
		return err
	}
	ids := map[string]string{} // test id -> file
	add := func(t *test) error {
		if other, ok := ids[t.id]; ok {
			return fmt.Errorf("%s: test %s is declared twice (also in %s)", t.path, t.id, other)
		}
		ids[t.id] = t.path
		p.tests = append(p.tests, t)
		return nil
	}
	for _, file := range files {
		rel, _ := filepath.Rel(p.dir, file)
		rel = filepath.ToSlash(rel)
		var f testFile
		if err := readYAML(file, &f, false); err != nil {
			return fmt.Errorf("%s: %w", rel, errors.Unwrap(err))
		}
		for _, mt := range f.Models {
			m := p.models[mt.Name]
			if m == nil {
				return fmt.Errorf("%s: there is no model %q", rel, mt.Name)
			}
			for _, c := range mt.Columns {
				if c.Name == "" {
					return fmt.Errorf("%s: model %s: a column without a name", rel, m.name)
				}
				for _, n := range c.Tests {
					t, err := p.columnTest(m, c.Name, &n)
					if err != nil {
						return fmt.Errorf("%s: line %d: %s.%s: %w", rel, n.Line, m.name, c.Name, err)
					}
					t.path = rel
					if err := add(t); err != nil {
						return err
					}
				}
			}
		}
	}

	var errs []string
	for _, name := range sortedKeys(p.singulars) {
		if err := p.singularTest(p.singulars[name], add); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	sort.Slice(p.tests, func(i, j int) bool { return p.tests[i].id < p.tests[j].id })
	return nil
}

var relationRE = regexp.MustCompile(`^\s*(ref|source)\(\s*(?:'([^']*)'|"([^"]*)")\s*(?:,\s*(?:'([^']*)'|"([^"]*)")\s*)?\)\s*$`)

// columnTest reads one entry of a column's tests: a name, or a one-key map
// from the name to its parameters.
func (p *project) columnTest(m *model, column string, n *yaml.Node) (*test, error) {
	var kind string
	params := map[string]any{}
	switch {
	case n.Kind == yaml.ScalarNode:
		kind = n.Value
	case n.Kind == yaml.MappingNode && len(n.Content) == 2:
		kind = n.Content[0].Value
		if err := n.Content[1].Decode(&params); err != nil {
			return nil, fmt.Errorf("%s: %w", kind, err)
		}
	default:
		return nil, fmt.Errorf("a test is a name, or a name with its parameters, like accepted_values: {values: [a, b]}")
	}

	t := &test{id: kind + "_" + m.name + "_" + column, models: []string{m.name}, severity: "error"}
	if s, ok := params["severity"]; ok {
		t.severity, _ = s.(string)
		if t.severity != "error" && t.severity != "warn" {
			return nil, fmt.Errorf("%s: severity %v: use error or warn", kind, s)
		}
		delete(params, "severity")
	}
	allowed := map[string][]string{"not_null": nil, "unique": nil, "accepted_values": {"values"}, "relationships": {"to", "field"}}
	want, ok := allowed[kind]
	if !ok {
		return nil, fmt.Errorf("unknown test %q; use not_null, unique, accepted_values or relationships (or a singular test in tests/)", kind)
	}
	for k := range params {
		if !slices.Contains(want, k) {
			return nil, fmt.Errorf("%s: unknown parameter %s", kind, k)
		}
	}
	for _, k := range want {
		if params[k] == nil {
			return nil, fmt.Errorf("%s: %s is required", kind, k)
		}
	}

	rel, col := p.relation(m), qi(column)
	switch kind {
	case "not_null":
		t.sql = fmt.Sprintf("SELECT * FROM %s WHERE %s IS NULL", rel, col)
	case "unique":
		t.sql = fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL GROUP BY %s HAVING count(*) > 1", col, rel, col, col)
	case "accepted_values":
		values, ok := params["values"].([]any)
		if !ok || len(values) == 0 {
			return nil, fmt.Errorf("accepted_values: values must be a list")
		}
		var lits []string
		for _, v := range values {
			switch v := v.(type) {
			case string:
				lits = append(lits, ql(v))
			case int, float64, bool:
				lits = append(lits, fmt.Sprint(v))
			default:
				return nil, fmt.Errorf("accepted_values: value %v: use strings, numbers or booleans", v)
			}
		}
		t.sql = fmt.Sprintf("SELECT * FROM %s WHERE %s IS NOT NULL AND %s NOT IN (%s)", rel, col, col, strings.Join(lits, ", "))
	case "relationships":
		to, _ := params["to"].(string)
		field, _ := params["field"].(string)
		mt := relationRE.FindStringSubmatch(to)
		if mt == nil {
			return nil, fmt.Errorf("relationships: to %q: use ref('model') or source('source', 'table')", to)
		}
		a, b := mt[2]+mt[3], mt[4]+mt[5]
		var target string
		switch {
		case mt[1] == "ref" && b == "":
			other := p.models[a]
			if other == nil {
				return nil, fmt.Errorf("relationships: to ref('%s'): there is no model %s", a, a)
			}
			if other != m {
				// The model is built after the one it checks against.
				m.testDeps = union(m.testDeps, []string{other.name})
			}
			target = p.relation(other)
		case mt[1] == "source" && b != "":
			schema := p.sources[a][b]
			if schema == "" {
				return nil, fmt.Errorf("relationships: to source('%s', '%s') is not declared in sources.yml", a, b)
			}
			target = schema + "." + b
		default:
			return nil, fmt.Errorf("relationships: to %q: use ref('model') or source('source', 'table')", to)
		}
		if field == "" {
			return nil, fmt.Errorf("relationships: field must be a column name")
		}
		t.sql = fmt.Sprintf("SELECT * FROM %s c WHERE c.%s IS NOT NULL AND NOT EXISTS (SELECT 1 FROM %s p WHERE p.%s = c.%s)",
			rel, col, target, qi(field), col)
	}
	return t, nil
}

// singularTest renders a test in tests/: a template whose rows are failures.
func (p *project) singularTest(m *model, add func(*test) error) error {
	rel := m.path
	res, err := p.r.render(m, parsePhase, false)
	if err != nil {
		return err
	}
	if len(res.refs) == 0 {
		return fmt.Errorf("%s: a test must ref() at least one model (tests on sources only are not supported)", rel)
	}
	m.refs, m.srcs = res.refs, res.srcs
	sql, err := p.compile(m, false)
	if err != nil {
		return err
	}
	severity := res.severity
	if severity == "" {
		severity = "error"
	}
	sql = strings.TrimRight(strings.TrimSpace(sql), ";")
	return add(&test{id: m.name, path: rel, severity: severity, models: m.refs, sql: sql, singular: m})
}
