package main

import (
	"fmt"
	"slices"
	"strings"
)

// selectModels returns the models sel picks, minus those excl picks, in DAG
// order. An empty sel picks every model. Both are space-separated terms,
// joined as a union:
//
//	orders           the model
//	+orders          and everything upstream
//	orders+          and everything downstream
//	+orders+         both
//	tag:daily        models with the tag (+ works here too)
//	path:models/stg  models under that folder, or that file (+ works here too)
//	source:raw.orders+  everything downstream of a source table; source:raw+ of any of its tables
//
// A term that matches nothing is an error, so a typo can't select nothing.
func (p *project) selectModels(sel, excl string) ([]*model, error) {
	picked := map[string]bool{}
	if strings.TrimSpace(sel) == "" {
		for name := range p.models {
			picked[name] = true
		}
	} else if err := p.pick(sel, picked); err != nil {
		return nil, fmt.Errorf("select: %w", err)
	}
	if strings.TrimSpace(excl) != "" {
		drop := map[string]bool{}
		if err := p.pick(excl, drop); err != nil {
			return nil, fmt.Errorf("exclude: %w", err)
		}
		for name := range drop {
			delete(picked, name)
		}
	}
	var out []*model
	for _, m := range p.order {
		if picked[m.name] {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the selection leaves no models")
	}
	return out, nil
}

// pick adds the models the terms of expr select to set.
func (p *project) pick(expr string, set map[string]bool) error {
	for _, term := range strings.Fields(expr) {
		base := term
		up := strings.HasPrefix(base, "+")
		base = strings.TrimPrefix(base, "+")
		down := strings.HasSuffix(base, "+")
		base = strings.TrimSuffix(base, "+")

		var names []string
		kind, arg, typed := strings.Cut(base, ":")
		switch {
		case !typed:
			if p.models[base] == nil {
				return fmt.Errorf("%s: there is no model %s", term, base)
			}
			names = []string{base}
		case kind == "tag":
			for _, m := range p.order {
				if slices.Contains(m.cfg.Tags, arg) {
					names = append(names, m.name)
				}
			}
		case kind == "path":
			arg = strings.TrimSuffix(arg, "/")
			for _, m := range p.order {
				if m.path == arg || strings.HasPrefix(m.path, arg+"/") {
					names = append(names, m.name)
				}
			}
		case kind == "source":
			src, table, one := strings.Cut(arg, ".")
			if p.sources[src] == nil || one && p.sources[src][table] == "" {
				return fmt.Errorf("%s: %s is not declared in sources.yml", term, arg)
			}
			if up || !down {
				return fmt.Errorf("%s: a source is not a model; select what reads it with source:%s+", term, arg)
			}
			for _, m := range p.order {
				for _, s := range m.srcs {
					if s == arg || !one && strings.HasPrefix(s, src+".") {
						names = append(names, m.name)
						break
					}
				}
			}
		default:
			return fmt.Errorf("%s: unknown selector %s:; use a model name, tag:, path: or source:", term, kind)
		}
		if len(names) == 0 {
			return fmt.Errorf("%s matches no models", term)
		}
		for _, n := range names {
			set[n] = true
		}
		if up {
			for _, n := range names {
				p.walk(n, func(m *model) []string { return m.deps() }, set)
			}
		}
		if down {
			users := p.users()
			for _, n := range names {
				p.walk(n, func(m *model) []string { return users[m.name] }, set)
			}
		}
	}
	return nil
}

// walk adds to set every model reachable from name through next.
func (p *project) walk(name string, next func(*model) []string, set map[string]bool) {
	seen := map[string]bool{name: true}
	queue := []string{name}
	for len(queue) > 0 {
		m := p.models[queue[0]]
		queue = queue[1:]
		for _, n := range next(m) {
			if !seen[n] {
				seen[n] = true
				set[n] = true
				queue = append(queue, n)
			}
		}
	}
}

// users returns, for each model, the models built after it (that ref it, or
// check against it in a relationships test).
func (p *project) users() map[string][]string {
	users := map[string][]string{}
	for _, m := range p.order {
		for _, d := range m.deps() {
			users[d] = append(users[d], m.name)
		}
	}
	return users
}
