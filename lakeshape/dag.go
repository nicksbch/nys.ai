package main

import (
	"fmt"
	"slices"
	"strings"
)

// sortModels returns the models with every model after the models it refs.
// Models whose dependencies are done at the same time come in name order, so
// the order is the same on every run. A cycle is an error that names it.
func sortModels(models map[string]*model) ([]*model, error) {
	waiting := map[string]int{} // model -> refs not yet in order
	users := map[string][]string{}
	for _, name := range sortedKeys(models) {
		m := models[name]
		deps := m.deps()
		waiting[name] = len(deps)
		for _, ref := range deps {
			users[ref] = append(users[ref], name)
		}
	}
	var ready, order []string
	for _, name := range sortedKeys(models) {
		if waiting[name] == 0 {
			ready = append(ready, name)
		}
	}
	for len(ready) > 0 {
		slices.Sort(ready)
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		for _, u := range users[name] {
			if waiting[u]--; waiting[u] == 0 {
				ready = append(ready, u)
			}
		}
	}
	if len(order) < len(models) {
		return nil, fmt.Errorf("models ref each other in a cycle: %s", findCycle(models, waiting))
	}
	out := make([]*model, len(order))
	for i, name := range order {
		out[i] = models[name]
	}
	return out, nil
}

// findCycle returns a cycle among the models still waiting, as "a → b → a",
// where a refs b (or a relationships test of a checks against b).
func findCycle(models map[string]*model, waiting map[string]int) string {
	const (
		unseen = iota
		onPath
		done
	)
	state := map[string]int{}
	var path []string
	var walk func(string) []string
	walk = func(name string) []string {
		state[name] = onPath
		path = append(path, name)
		for _, ref := range models[name].deps() {
			switch state[ref] {
			case onPath:
				i := slices.Index(path, ref)
				return append(slices.Clone(path[i:]), ref)
			case unseen:
				if c := walk(ref); c != nil {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		state[name] = done
		return nil
	}
	for _, name := range sortedKeys(waiting) {
		if waiting[name] > 0 && state[name] == unseen {
			if c := walk(name); c != nil {
				return strings.Join(c, " → ")
			}
		}
	}
	return "(not found)"
}
