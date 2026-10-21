package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/nikolalohinski/gonja/v2"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
)

// A model is rendered in two phases. The parse phase finds its config and
// dependencies; ref() and this give placeholders, because the schemas of the
// models they name are only known once every model is parsed. The compile
// phase gives the SQL to run.
type phase int

const (
	parsePhase phase = iota
	compilePhase
)

// Names the renderer defines for every model. Macros may not reuse them.
var builtinNames = []string{"ref", "source", "config", "var", "is_incremental", "this", "target"}

type renderer struct {
	p      *project
	cfg    *config.Config
	loader loaders.Loader
	// prefix imports every macro. It is put in front of each model on its
	// first line, with no newline, so error line numbers stay right.
	prefix string
}

// rendered is what one render of a model gives.
type rendered struct {
	sql      string
	cfg      modelConfig // what its config() calls set
	severity string      // what config() set, for a singular test
	refs     []string
	srcs     []string
}

func newRenderer(p *project) *renderer {
	cfg := config.New()
	cfg.StrictUndefined = true // an undefined name is an error, not empty SQL
	return &renderer{p: p, cfg: cfg}
}

// findMacros reads the names of the macros in macros/*.sql, and sets up the
// loader the models are rendered from.
func (r *renderer) findMacros() error {
	files := map[string]string{}
	for _, mf := range r.p.macros {
		b, err := readFile(r.p.dir, mf.path)
		if err != nil {
			return err
		}
		files["/"+mf.path] = b
	}
	loader, err := loaders.NewMemoryLoader(withRoot(files))
	if err != nil {
		return err
	}
	defined := map[string]string{} // macro -> file
	for i, mf := range r.p.macros {
		t, err := exec.NewTemplate("/"+mf.path, r.cfg, loader, r.env(nil))
		if err != nil {
			return fmt.Errorf("%s: %s", mf.path, templateError(err))
		}
		for name := range t.Macros() {
			if slices.Contains(builtinNames, name) {
				return fmt.Errorf("%s: macro %s: the name is taken by lakeshape's own %s()", mf.path, name, name)
			}
			if other, ok := defined[name]; ok {
				return fmt.Errorf("macro %s is defined twice: in %s and %s", name, other, mf.path)
			}
			defined[name] = mf.path
			r.p.macros[i].names = append(r.p.macros[i].names, name)
		}
		sort.Strings(r.p.macros[i].names)
		if len(r.p.macros[i].names) > 0 {
			r.prefix += fmt.Sprintf("{%%- from '/%s' import %s -%%}", mf.path, strings.Join(r.p.macros[i].names, ", "))
		}
	}
	for _, m := range r.p.models {
		files["/"+m.path] = r.prefix + m.raw
	}
	for _, m := range r.p.singulars {
		files["/"+m.path] = r.prefix + m.raw
	}
	r.loader, err = loaders.NewMemoryLoader(withRoot(files))
	return err
}

// withRoot adds a placeholder at "/" so the memory loader's root is "/",
// whatever files there are.
func withRoot(files map[string]string) map[string]string {
	files["/"] = ""
	return files
}

func (r *renderer) env(ctx map[string]any) *exec.Environment {
	d := gonja.DefaultEnvironment
	return &exec.Environment{
		Context:           d.Context.Inherit().Update(exec.NewContext(ctx)),
		Filters:           d.Filters,
		Tests:             d.Tests,
		ControlStructures: d.ControlStructures,
		Methods:           d.Methods,
	}
}

// render renders model m. incremental is what is_incremental() returns.
func (r *renderer) render(m *model, ph phase, incremental bool) (*rendered, error) {
	p := r.p
	res := &rendered{}
	ctx := map[string]any{
		"ref": func(va *exec.VarArgs) (*exec.Value, error) {
			name, err := stringArgs(va, "ref('model')", 1)
			if err != nil {
				return nil, err
			}
			dep := p.models[name[0]]
			switch {
			case dep == nil:
				return nil, fmt.Errorf("ref('%s'): there is no model %s", name[0], name[0])
			case dep == m:
				return nil, fmt.Errorf("ref('%s'): a model can't ref itself; use {{ this }}", name[0])
			}
			res.refs = union(res.refs, name)
			if ph == parsePhase {
				return exec.AsValue(dep.name), nil
			}
			return exec.AsValue(p.relation(dep)), nil
		},
		"source": func(va *exec.VarArgs) (*exec.Value, error) {
			args, err := stringArgs(va, "source('source', 'table')", 2)
			if err != nil {
				return nil, err
			}
			schema := p.sources[args[0]][args[1]]
			if schema == "" {
				return nil, fmt.Errorf("source('%s', '%s') is not declared in sources.yml", args[0], args[1])
			}
			res.srcs = union(res.srcs, []string{args[0] + "." + args[1]})
			return exec.AsValue(schema + "." + args[1]), nil
		},
		"config": func(va *exec.VarArgs) (*exec.Value, error) {
			if m.test {
				s, err := testConfigArgs(va)
				if err != nil {
					return nil, err
				}
				res.severity = s
				return exec.AsValue(""), nil
			}
			c, err := configArgs(va)
			if err != nil {
				return nil, err
			}
			res.cfg = res.cfg.over(c)
			return exec.AsValue(""), nil
		},
		"var": func(va *exec.VarArgs) (*exec.Value, error) {
			if len(va.Args) < 1 || len(va.Args) > 2 || len(va.KwArgs) > 0 || !va.Args[0].IsString() {
				return nil, errors.New("use var('name') or var('name', default)")
			}
			k := va.Args[0].String()
			if v, ok := p.Vars[k]; ok {
				return exec.AsValue(v), nil
			}
			if len(va.Args) == 2 {
				return va.Args[1], nil
			}
			return nil, fmt.Errorf("var('%s') is not set in project.yaml or -vars, and has no default", k)
		},
		"is_incremental": func(va *exec.VarArgs) (*exec.Value, error) {
			if len(va.Args) > 0 || len(va.KwArgs) > 0 {
				return nil, errors.New("is_incremental() takes no arguments")
			}
			return exec.AsValue(incremental), nil
		},
		"target": map[string]any{"name": p.target.name},
	}
	switch {
	case m.test: // a test has no relation of its own
	case ph == parsePhase:
		ctx["this"] = m.name
	default:
		ctx["this"] = p.relation(m)
	}

	t, err := exec.NewTemplate("/"+m.path, r.cfg, r.loader, r.env(ctx))
	if err != nil {
		return nil, fmt.Errorf("%s: %s", m.path, templateError(err))
	}
	res.sql, err = t.ExecuteToString(nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", m.path, templateError(err))
	}
	return res, nil
}

var (
	lineRE  = regexp.MustCompile(`Unable to render expression at line (\d+): `)
	callRE  = regexp.MustCompile(`call\(.*?\): invalid call to function '[^']*': `)
	macroRE = regexp.MustCompile(`Unable to execute macro '([^']*)': `)
)

// templateError drops gonja's wrapping, which repeats the whole template
// source for parse errors and the arguments of failed calls.
func templateError(err error) string {
	if u := errors.Unwrap(err); u != nil && strings.HasPrefix(err.Error(), "failed to parse template") {
		err = u
	}
	s := strings.TrimPrefix(err.Error(), "unable to execute template: ")
	s = lineRE.ReplaceAllString(s, "line $1: ")
	s = macroRE.ReplaceAllString(s, "in macro $1, ")
	return callRE.ReplaceAllString(s, "")
}

// stringArgs returns exactly n positional string arguments, or an error showing usage.
func stringArgs(va *exec.VarArgs, usage string, n int) ([]string, error) {
	if len(va.Args) != n || len(va.KwArgs) > 0 {
		return nil, fmt.Errorf("use %s", usage)
	}
	out := make([]string, n)
	for i, a := range va.Args {
		if !a.IsString() {
			return nil, fmt.Errorf("use %s, with strings", usage)
		}
		out[i] = a.String()
	}
	return out, nil
}

// configArgs reads the keyword arguments of a config() call.
func configArgs(va *exec.VarArgs) (modelConfig, error) {
	var c modelConfig
	if len(va.Args) > 0 {
		return c, errors.New("config() takes keyword arguments only, e.g. config(materialized='table')")
	}
	for _, k := range sortedKeys(va.KwArgs) {
		v := va.KwArgs[k]
		var err error
		switch k {
		case "materialized":
			c.Materialized, err = configString(k, v)
		case "schema":
			c.Schema, err = configString(k, v)
		case "incremental_strategy":
			c.IncrementalStrategy, err = configString(k, v)
		case "unique_key":
			c.UniqueKey, err = configList(k, v)
		case "tags":
			c.Tags, err = configList(k, v)
		default:
			err = fmt.Errorf("config(): unknown key %s; use materialized, schema, unique_key, incremental_strategy or tags", k)
		}
		if err != nil {
			return c, err
		}
	}
	if err := checkConfig(c); err != nil {
		return c, fmt.Errorf("config(): %w", err)
	}
	return c, nil
}

// testConfigArgs reads a config() call in a singular test: only severity.
func testConfigArgs(va *exec.VarArgs) (string, error) {
	v, ok := va.KwArgs["severity"]
	if len(va.Args) > 0 || len(va.KwArgs) != 1 || !ok {
		return "", errors.New("config() in a test takes only severity, e.g. config(severity='warn')")
	}
	if s := v.String(); v.IsString() && (s == "error" || s == "warn") {
		return s, nil
	}
	return "", errors.New("config(): severity must be 'error' or 'warn'")
}

func configString(k string, v *exec.Value) (string, error) {
	if !v.IsString() || v.String() == "" {
		return "", fmt.Errorf("config(): %s must be a non-empty string", k)
	}
	return v.String(), nil
}

func configList(k string, v *exec.Value) ([]string, error) {
	if v.IsString() {
		return []string{v.String()}, nil
	}
	if !v.IsList() {
		return nil, fmt.Errorf("config(): %s must be a string or a list of strings", k)
	}
	list, ok := v.ToGoSimpleType(false).([]any)
	if !ok {
		return nil, fmt.Errorf("config(): %s must be a string or a list of strings", k)
	}
	out := []string{}
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("config(): %s must be a string or a list of strings", k)
		}
		out = append(out, s)
	}
	return out, nil
}
