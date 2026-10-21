package main

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var dev = target{name: "dev", user: "nicolas"}

// writeProject writes a project with a minimal project.yaml and sources.yml,
// plus files (path -> content; an empty content removes a base file).
func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	base := map[string]string{
		"project.yaml": "name: test\nschema: analytics\n",
		"sources.yml":  "sources:\n  - name: raw\n    tables: [orders]\n",
	}
	for k, v := range files {
		base[k] = v
	}
	os.MkdirAll(filepath.Join(dir, "models"), 0o755)
	for k, v := range base {
		if v == "" {
			continue
		}
		f := filepath.Join(dir, k)
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func names(ms []*model) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.name)
	}
	return out
}

func TestShop(t *testing.T) {
	p, err := loadProject("testdata/shop", dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(p.order), []string{"stg_customers", "stg_orders", "orders", "recent", "revenue"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}

	orders := p.models["orders"]
	if got := p.relation(orders); got != "dev_nicolas__marts.orders" {
		t.Errorf("orders relation = %s", got)
	}
	if c := orders.cfg; c.Materialized != "incremental" || c.IncrementalStrategy != "delete+insert" || !slices.Equal(c.UniqueKey, []string{"order_id"}) {
		t.Errorf("orders config = %+v", c)
	}
	if got := p.models["revenue"]; got.cfg.Schema != "finance" || got.cfg.Materialized != "table" || !slices.Equal(got.cfg.Tags, []string{"daily", "finance"}) {
		t.Errorf("revenue config = %+v, want deepest folder's schema, marts' materialization, tags added up", got.cfg)
	}
	if got := p.models["recent"].refs; !slices.Equal(got, []string{"stg_orders"}) {
		t.Errorf("recent refs = %v, want the ref made inside a macro", got)
	}
	if got := p.models["stg_orders"].srcs; !slices.Equal(got, []string{"raw.orders"}) {
		t.Errorf("stg_orders sources = %v", got)
	}

	full, err := p.compile(orders, false)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := p.compile(orders, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "from dev_nicolas__staging.stg_orders o") || strings.Contains(full, "max(ordered_at)") {
		t.Errorf("first build SQL:\n%s", full)
	}
	if !strings.Contains(inc, "(select max(ordered_at) from dev_nicolas__marts.orders)") {
		t.Errorf("incremental SQL:\n%s", inc)
	}
	sql, _ := p.compile(p.models["stg_orders"], false)
	if !strings.Contains(sql, "(amount_cents / 100.0) as amount") || !strings.Contains(sql, "from raw.orders") {
		t.Errorf("stg_orders SQL:\n%s", sql)
	}

	prod, err := loadProject("testdata/shop", target{name: "prod"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := prod.relation(prod.models["orders"]); got != "marts.orders" {
		t.Errorf("prod orders relation = %s", got)
	}
}

func TestVars(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"project.yaml":  "name: test\nvars: {a: 1, b: x}\n",
		"models/m.sql":  "select {{ var('a') }}, '{{ var('b') }}', {{ var('c', 3) }}",
		"models/n.sql":  "select '{{ target.name }}'",
		"models/o.sql":  "{% if target.name == 'dev' %}{{ config(materialized='table') }}{% endif %}select 1",
		"models/p.sql":  "{% if is_incremental() %}select * from {{ ref('n') }}{% else %}select 1{% endif %}",
		"models/p2.sql": "{{ config(materialized='incremental') }}{% if is_incremental() %}select * from {{ ref('n') }}{% else %}select 1{% endif %}",
	})
	p, err := loadProject(dir, dev, map[string]any{"b": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if sql, _ := p.compile(p.models["m"], false); sql != "select 1, 'y', 3" {
		t.Errorf("vars: %q", sql)
	}
	if sql, _ := p.compile(p.models["n"], false); sql != "select 'dev'" {
		t.Errorf("target: %q", sql)
	}
	if p.models["o"].cfg.Materialized != "table" {
		t.Errorf("config by target: %+v", p.models["o"].cfg)
	}
	if got := p.models["p"].refs; len(got) != 0 {
		t.Errorf("view p refs = %v: is_incremental() is never true for a view", got)
	}
	if got := p.models["p2"].refs; !slices.Equal(got, []string{"n"}) {
		t.Errorf("incremental p2 refs = %v, want the ref made only when incremental", got)
	}
	if got := p.models["main"]; got != nil || p.models["m"].cfg.Schema != "main" {
		t.Errorf("default schema = %q, want main", p.models["m"].cfg.Schema)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"missing ref", map[string]string{"models/a.sql": "select * from {{ ref('nope') }}"}, "models/a.sql: line 1: ref('nope'): there is no model nope"},
		{"self ref", map[string]string{"models/a.sql": "select * from {{ ref('a') }}"}, "use {{ this }}"},
		{"ref usage", map[string]string{"models/a.sql": "select * from {{ ref('x', 'y') }}"}, "use ref('model')"},
		{"cycle", map[string]string{
			"models/a.sql": "select * from {{ ref('b') }}",
			"models/b.sql": "select * from {{ ref('c') }}",
			"models/c.sql": "select * from {{ ref('a') }}",
			"models/d.sql": "select * from {{ ref('a') }}",
		}, "cycle: a → b → c → a"},
		{"undeclared source", map[string]string{"models/a.sql": "select * from {{ source('raw', 'nope') }}"}, "source('raw', 'nope') is not declared in sources.yml"},
		{"unknown config key", map[string]string{"models/a.sql": "{{ config(materialised='table') }}select 1"}, "unknown key materialised"},
		{"bad materialized", map[string]string{"models/a.sql": "{{ config(materialized='ephemeral') }}select 1"}, `materialized "ephemeral"`},
		{"positional config", map[string]string{"models/a.sql": "{{ config('table') }}select 1"}, "keyword arguments only"},
		{"duplicate model", map[string]string{"models/x/a.sql": "select 1", "models/y/a.sql": "select 2"}, "model a is defined twice: models/x/a.sql and models/y/a.sql"},
		{"bad model name", map[string]string{"models/my-model.sql": "select 1"}, "models/my-model.sql: model names"},
		{"undefined name", map[string]string{"macros/m.sql": "{% macro x() %}1{% endmacro %}", "models/a.sql": "select 1\nfrom t\nwhere {{ nope }}"}, "models/a.sql: line 3:"},
		{"parse error", map[string]string{"models/a.sql": "select 1\n{% if %}"}, `models/a.sql: Unable to parse controlStructure "if"`},
		{"unset var", map[string]string{"models/a.sql": "select {{ var('x') }}"}, "var('x') is not set"},
		{"delete+insert without key", map[string]string{"models/a.sql": "{{ config(materialized='incremental', incremental_strategy='delete+insert') }}select 1"}, "needs a unique_key"},
		{"reserved schema", map[string]string{"models/a.sql": "{{ config(schema='lakeshape') }}select 1"}, "reserved for lakeshape"},
		{"dev schema in config", map[string]string{"models/a.sql": "{{ config(schema='dev_x__y') }}select 1"}, "reserved for dev targets"},
		{"dev schema in project", map[string]string{"project.yaml": "name: t\nschema: dev_me\n"}, "project.yaml: schema:"},
		{"missing folder", map[string]string{"project.yaml": "name: t\nmodels:\n  stagin: {schema: s}\n"}, `folder "stagin": there is no models/stagin folder`},
		{"unknown project key", map[string]string{"project.yaml": "name: t\nschemas: x\n"}, "field schemas not found"},
		{"no name", map[string]string{"project.yaml": "schema: x\n"}, "name is required"},
		{"macro shadows builtin", map[string]string{"macros/m.sql": "{% macro ref(x) %}{% endmacro %}"}, "macro ref: the name is taken"},
		{"macro twice", map[string]string{"macros/a.sql": "{% macro x() %}{% endmacro %}", "macros/b.sql": "{% macro x() %}{% endmacro %}"}, "macro x is defined twice: in macros/a.sql and macros/b.sql"},
		{"model is a source", map[string]string{"models/orders.sql": "{{ config(schema='raw') }}select 1"}, "which is source('raw', 'orders')"},
		{"config by is_incremental", map[string]string{"models/a.sql": "{% if is_incremental() %}{{ config(materialized='incremental', schema='x') }}{% else %}{{ config(materialized='incremental') }}{% endif %}select 1"}, "whether or not is_incremental()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadProject(writeProject(t, tc.files), dev, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v\nwant it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCompileMustMatchParse(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"models/a.sql": "select 1",
		"models/b.sql": "{% if ref('a') == 'a' %}{{ config(materialized='table') }}{% endif %}select 2",
	})
	p, err := loadProject(dir, dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.compile(p.models["b"], false); err == nil || !strings.Contains(err.Error(), "must not") {
		t.Errorf("error = %v", err)
	}
}

func TestDevUser(t *testing.T) {
	for in, want := range map[string]string{"nicolas": "nicolas", "Nicolas.Steinbach": "nicolas_steinbach", "a-b c": "a_b_c"} {
		if got, err := devUser(in); err != nil || got != want {
			t.Errorf("devUser(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "...", "ü"} {
		if _, err := devUser(in); err == nil {
			t.Errorf("devUser(%q): want an error", in)
		}
	}
}

func TestCheckFlags(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		args []string
		ok   bool
	}{
		{"run", []string{"-dev-user", "nic", "-select", "a+", "-full-refresh"}, true},
		{"ls", []string{"-target", "prod", "-orphans"}, true},
		{"trigger", []string{"-full-refresh", "-project", "p", "-duckdb", "d", "-state-dir", "s"}, true},
		{"serve", []string{"-project", "p"}, true},
		{"trigger", []string{"-dev-user", "nic"}, false},
		{"trigger", []string{"-select", "a"}, false},
		{"serve", []string{"-target", "dev"}, false},
		{"compile", []string{"-full-refresh"}, false},
		{"run", []string{"-orphans"}, false},
		{"history", []string{"-vars", "{}"}, false},
	} {
		fs := flag.NewFlagSet("lakeshape", flag.ContinueOnError)
		for _, name := range []string{"project", "target", "dev-user", "vars", "select", "exclude", "duckdb", "state-dir"} {
			fs.String(name, "", "")
		}
		fs.Bool("full-refresh", false, "")
		fs.Bool("orphans", false, "")
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := checkFlags(fs, tc.cmd); (err == nil) != tc.ok {
			t.Errorf("%s %v: got %v, want ok=%v", tc.cmd, tc.args, err, tc.ok)
		}
	}
}

func TestSelect(t *testing.T) {
	p, err := loadProject("testdata/shop", dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sel, excl, want string }{
		{"", "", "stg_customers stg_orders orders recent revenue"},
		{"orders", "", "orders"},
		{"+orders", "", "stg_customers stg_orders orders"},
		{"orders+", "", "orders revenue"},
		{"+orders+", "", "stg_customers stg_orders orders revenue"},
		{"stg_orders+", "", "stg_orders orders recent revenue"},
		{"tag:finance", "", "revenue"},
		{"tag:daily", "", "orders recent revenue"},
		{"+tag:finance", "", "stg_customers stg_orders orders revenue"},
		{"path:models/staging", "", "stg_customers stg_orders"},
		{"path:models/staging/", "", "stg_customers stg_orders"},
		{"path:models/marts/recent.sql", "", "recent"},
		{"path:models/marts", "tag:finance", "orders recent"},
		{"source:raw.customers+", "", "stg_customers orders revenue"},
		{"source:raw+", "", "stg_customers stg_orders orders recent revenue"},
		{"recent revenue", "", "recent revenue"},
		{"stg_orders +revenue", "stg_customers", "stg_orders orders revenue"},
		{"", "tag:staging", "orders recent revenue"},
	} {
		ms, err := p.selectModels(tc.sel, tc.excl)
		if err != nil {
			t.Errorf("select %q exclude %q: %v", tc.sel, tc.excl, err)
			continue
		}
		if got := strings.Join(names(ms), " "); got != tc.want {
			t.Errorf("select %q exclude %q = %s, want %s", tc.sel, tc.excl, got, tc.want)
		}
	}
	for sel, want := range map[string]string{
		"nope":              "there is no model nope",
		"tag:nope":          "tag:nope matches no models",
		"path:models/x":     "matches no models",
		"source:raw.nope+":  "not declared in sources.yml",
		"source:raw.orders": "a source is not a model",
		"+source:raw+":      "a source is not a model",
		"kind:x":            "unknown selector kind:",
	} {
		if _, err := p.selectModels(sel, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("select %q: error %v, want %q", sel, err, want)
		}
	}
	if _, err := p.selectModels("orders", "orders"); err == nil || !strings.Contains(err.Error(), "leaves no models") {
		t.Errorf("empty selection: %v", err)
	}
}
