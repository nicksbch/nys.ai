package main

// suggest_key: where a download belongs. Files of the same kind are kept
// together, so the folders that already hold files named like this one
// (same extension, digits in the same places: 2023-08.zip is ####-##.zip)
// are the likely places for it. This is plain matching, so the same URL
// always gets the same answer, and a small model only has to copy a key.

import (
	"path"
	"sort"
	"strings"
	"time"
)

const (
	maxScanned     = 20000 // objects suggest_key reads from a bucket
	maxSuggestions = 5
	maxSimilar     = 3 // examples per suggestion
)

type suggestion struct {
	Key     string   `json:"key"`     // the full key to download to
	Folder  string   `json:"folder"`  // where it is
	Match   string   `json:"match"`   // "name pattern" or "extension" (weaker)
	Count   int      `json:"count"`   // files there that match
	Similar []string `json:"similar"` // some of them, newest first
	Exists  bool     `json:"exists"`  // a file is already at key
}

// shape is a file name with every digit replaced by #, lower-cased.
func shape(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= '0' && r <= '9' {
			r = '#'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// suggest ranks the folders holding files like file: by the same name
// pattern if any match, else by the same extension; most matches first,
// then the most recently changed.
func suggest(objs []object, file string) []suggestion {
	want, ext := shape(file), strings.ToLower(path.Ext(file))
	type folder struct {
		s      suggestion
		newest time.Time
		files  []object
	}
	byMatch := map[string]map[string]*folder{"name pattern": {}, "extension": {}}
	keys := map[string]bool{}
	for _, o := range objs {
		keys[o.Key] = true
		name := path.Base(o.Key)
		if strings.HasSuffix(o.Key, "/") || name == "" {
			continue
		}
		match := ""
		switch {
		case shape(name) == want:
			match = "name pattern"
		case ext != "" && strings.ToLower(path.Ext(name)) == ext:
			match = "extension"
		default:
			continue
		}
		dir := path.Dir(o.Key) + "/"
		if dir == "./" {
			dir = ""
		}
		f := byMatch[match][dir]
		if f == nil {
			f = &folder{s: suggestion{Key: dir + file, Folder: dir, Match: match}}
			byMatch[match][dir] = f
		}
		f.s.Count++
		f.files = append(f.files, o)
		if o.Modified.After(f.newest) {
			f.newest = o.Modified
		}
	}
	found := byMatch["name pattern"]
	if len(found) == 0 {
		found = byMatch["extension"]
	}
	list := make([]*folder, 0, len(found))
	for _, f := range found {
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].s.Count != list[j].s.Count {
			return list[i].s.Count > list[j].s.Count
		}
		return list[i].newest.After(list[j].newest)
	})
	out := []suggestion{}
	for _, f := range list {
		if len(out) == maxSuggestions {
			break
		}
		sort.Slice(f.files, func(i, j int) bool { return f.files[i].Modified.After(f.files[j].Modified) })
		f.s.Similar = []string{}
		for _, o := range f.files[:min(len(f.files), maxSimilar)] {
			f.s.Similar = append(f.s.Similar, o.Key)
		}
		f.s.Exists = keys[f.s.Key]
		out = append(out, f.s)
	}
	return out
}
