package main

// Working out the links between the services from their configs, and the
// live state from relay.

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

type node struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Label   string   `json:"label"`
	Sub     string   `json:"sub,omitempty"`
	Service string   `json:"service"` // whose page the node links to
	Col     int      `json:"col"`
	Row     int      `json:"row"`
	At      *loc     `json:"at,omitempty"`
	Info    []string `json:"info,omitempty"`
	OnFail  []string `json:"on_fail,omitempty"` // foreman task types that wake up when it fails
	Error   string   `json:"error,omitempty"`
	Live    *live    `json:"live,omitempty"`
}

type edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"` // files, event, table, tools, observed
	Label string `json:"label,omitempty"`
	Live  *live  `json:"live,omitempty"`
}

// live is the last relay event of a node or an event edge.
type live struct {
	Topic string    `json:"topic"`
	Type  string    `json:"type"`
	Time  time.Time `json:"time"`
	Error string    `json:"error,omitempty"`
}

type graph struct {
	Nodes    []*node   `json:"nodes"`
	Edges    []*edge   `json:"edges"`
	Problems []problem `json:"problems"`
	Files    []*file   `json:"files"`
	Relay    string    `json:"relay_error,omitempty"`
	Built    time.Time `json:"built"`
}

// lsModel is one model as `lakeshape ls` lists it.
type lsModel struct {
	Relation string
	Deps     []string // other models, and source:<source>.<table>
}

// lsFunc lists the models a job's selection picks.
type lsFunc func(sel, exclude string) ([]lsModel, error)

// observedWindow is how far back url-to-bucket's downloads count as links.
const observedWindow = 30 * 24 * time.Hour

type builder struct {
	g     *graph
	nodes map[string]*node
	edges map[string]*edge
	rc    *recent
	now   time.Time
}

func build(c *configs, rc *recent, ls lsFunc, now time.Time) *graph {
	b := &builder{g: &graph{Files: c.Files, Relay: rc.Err, Built: now}, nodes: map[string]*node{}, edges: map[string]*edge{}, rc: rc, now: now}
	b.g.Problems = append(b.g.Problems, c.Problems...)

	b.storage(c)
	b.lakeshape(c, ls)
	b.serving(c)
	b.agents(c)
	b.live()
	b.layout()
	return b.g
}

func (b *builder) node(id, kind, label, service string, at *loc) *node {
	if n := b.nodes[id]; n != nil {
		if n.At == nil {
			n.At = at
		}
		return n
	}
	n := &node{ID: id, Kind: kind, Label: label, Service: service, At: at}
	b.nodes[id] = n
	b.g.Nodes = append(b.g.Nodes, n)
	return n
}

func (b *builder) edge(from, to, kind, label string) *edge {
	k := from + "\x00" + to + "\x00" + kind + "\x00" + label
	if e := b.edges[k]; e != nil {
		return e
	}
	e := &edge{From: from, To: to, Kind: kind, Label: label}
	b.edges[k] = e
	b.g.Edges = append(b.g.Edges, e)
	return e
}

func (b *builder) problem(at loc, format string, args ...any) {
	b.g.Problems = append(b.g.Problems, problem{File: at.File, Line: at.Line, Text: fmt.Sprintf(format, args...)})
}

// seen reports whether relay has seen a topic the filter matches. Without
// relay it can't tell, and says yes.
func (b *builder) seen(filter string) bool {
	if b.rc.Err != "" {
		return true
	}
	for _, t := range b.rc.Topics {
		if topicMatch(filter, t) {
			return true
		}
	}
	return false
}

func ptr(l loc) *loc { return &l }

// --- files: url-to-bucket, preprocessor, duckpipe ----------------------------

func prefixID(bucket, prefix string) string { return "s3://" + bucket + "/" + prefix }

// reader is something that picks files up from a prefix.
type reader struct {
	id, bucket, prefix, pattern string
	writes                      string // the prefix node it writes to, for preprocessor jobs
}

func (b *builder) storage(c *configs) {
	u2b := b.node("url-to-bucket", "download", "url-to-bucket", "url-to-bucket", nil)
	u2b.Sub = "downloads asked for on relay"

	prefix := func(bucket, p string) string {
		id := prefixID(bucket, p)
		b.node(id, "prefix", bucket+"/"+p, "garage-ui", nil)
		return id
	}
	var readers []reader
	for _, j := range c.PreJobs {
		n := b.node("unzip:"+j.Name, "unzip", j.Name, "preprocessor", ptr(j.at))
		n.Sub = "unzip " + j.Pattern
		in, out := prefix(j.Bucket, j.Prefix), prefix(j.Bucket, j.To)
		b.edge(in, n.ID, "files", j.Pattern)
		b.edge(n.ID, out, "files", "")
		readers = append(readers, reader{n.ID, j.Bucket, j.Prefix, j.Pattern, out})
	}
	for _, p := range c.Pipes {
		n := b.node("pipe:"+p.Name, "pipe", p.Name, "duckpipe", ptr(p.at))
		n.Sub = "→ " + p.Table
		b.edge(prefix(p.Bucket, p.Prefix), n.ID, "files", p.Pattern)
		b.edge(n.ID, b.table(p.Table, nil).ID, "table", "")
		readers = append(readers, reader{id: n.ID, bucket: p.Bucket, prefix: p.Prefix, pattern: p.Pattern})
	}

	// A prefix a job writes feeds every reader of the same prefix, or one
	// above or below it, other than the job itself.
	fed := map[string]bool{}
	for _, w := range readers {
		if w.writes == "" {
			continue
		}
		wrote := false
		for _, r := range readers {
			rid := prefixID(r.bucket, r.prefix)
			if r.id == w.id || r.bucket != w.bucket {
				continue
			}
			if !strings.HasPrefix(rid, w.writes) && !strings.HasPrefix(w.writes, rid) {
				continue
			}
			wrote = true
			fed[r.id] = true
			if rid != w.writes {
				b.edge(w.writes, rid, "files", "")
			}
		}
		if !wrote {
			at := *b.nodes[w.id].At
			b.problem(at, "preprocessor job %s writes to %s, which no pipe or job reads", b.nodes[w.id].Label, strings.TrimPrefix(w.writes, "s3://"))
		}
	}

	// Observed: url-to-bucket's downloads that a reader would pick up.
	counts := map[string]int{}
	total, unread := 0, 0
	for _, ev := range b.rc.Downs {
		if b.now.Sub(ev.Time) > observedWindow {
			break
		}
		bucket, _ := ev.Data["bucket"].(string)
		key, _ := ev.Data["key"].(string)
		total++
		read := false
		for _, r := range readers {
			if r.bucket == bucket && strings.HasPrefix(key, r.prefix) && patternMatch(r.pattern, strings.TrimPrefix(key, r.prefix)) {
				counts[prefixID(r.bucket, r.prefix)]++
				fed[r.id] = true
				read = true
			}
		}
		if !read {
			unread++
		}
	}
	for id, n := range counts {
		b.edge(u2b.ID, id, "observed", plural(n, "download")+", 30 days")
	}
	if b.rc.Err == "" {
		u2b.Info = append(u2b.Info, plural(total, "download")+" in the last 30 days")
		if unread > 0 {
			u2b.Info = append(u2b.Info, fmt.Sprintf("%d of them went where no job or pipe reads", unread))
		}
		for _, p := range c.Pipes {
			if !fed["pipe:"+p.Name] {
				b.problem(p.at, "pipe %s reads %s/%s, which no preprocessor job writes to and no download reached in 30 days", p.Name, p.Bucket, p.Prefix)
			}
		}
	}
}

// patternMatch is duckpipe's and preprocessor's: a glob on the file name, or,
// with a "/", on the key below the prefix. Empty matches everything.
func patternMatch(pattern, rel string) bool {
	if pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "/") {
		rel = path.Base(rel)
	}
	ok, _ := path.Match(pattern, rel)
	return ok
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

// table returns the node of a lake table, schema.table.
func (b *builder) table(name string, at *loc) *node {
	id := tableID(name)
	return b.node("table:"+id, "table", id, "lake-ui", at)
}

// --- lakeshape ---------------------------------------------------------------

func (b *builder) lakeshape(c *configs, ls lsFunc) {
	loaded := map[string][]string{} // table → the pipes that load it
	for _, p := range c.Pipes {
		t := tableID(p.Table)
		loaded[t] = append(loaded[t], "pipe:"+p.Name)
	}
	sources := map[string]string{} // source:<source>.<table> → schema.table
	for _, s := range c.Sources {
		t := s.Schema + "." + s.Table
		sources["source:"+s.Name+"."+s.Table] = t
		if len(loaded[t]) == 0 {
			b.problem(s.at, "sources.yml lists %s, which no pipe loads", t)
		}
	}
	for _, m := range c.Marts {
		n := b.table("marts."+m.Name, ptr(m.at))
		n.Kind = "mart"
	}

	for _, j := range c.LsJobs {
		n := b.node("job:"+j.Name, "job", j.Name, "lakeshape", ptr(j.at))
		n.Sub = j.Select
		if j.Schedule != "" {
			n.Info = append(n.Info, "schedule: "+j.Schedule)
		}
		if ls != nil {
			models, err := ls(j.Select, j.Exclude)
			if err != nil {
				n.Error = "lakeshape ls: " + err.Error()
			}
			for _, m := range models {
				for _, d := range m.Deps {
					if t, ok := sources[d]; ok {
						b.edge(b.table(t, nil).ID, n.ID, "table", "")
					}
				}
				if strings.HasPrefix(m.Relation, "marts.") {
					b.edge(n.ID, b.table(m.Relation, nil).ID, "table", "")
				}
			}
		}
		for _, t := range j.On {
			label := strings.TrimSpace(t.Topic + " " + t.Type + " " + dataLabel(t.Data))
			if !b.seen(t.Topic) {
				b.problem(j.at, "job %s waits for %s, a topic relay has never seen", j.Name, t.Topic)
			}
			var from []string
			if tbl, ok := t.Data["table"].(string); ok {
				from = loaded[tableID(tbl)]
				if len(from) == 0 {
					b.problem(j.at, "job %s waits for loads of %s, which no pipe loads", j.Name, tbl)
				}
			} else if p, ok := t.Data["pipe"].(string); ok {
				from = []string{"pipe:" + p}
			} else {
				from = b.servicesNodes(app(t.Topic))
			}
			for _, f := range from {
				if b.nodes[f] == nil {
					continue
				}
				e := b.edge(f, n.ID, "event", label)
				e.Live = b.lastMatching(t)
			}
		}
	}
}

func dataLabel(d map[string]any) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %v", k, d[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// servicesNodes returns the nodes of the service that publishes a topic's
// app segment ("*" is every service with nodes).
func (b *builder) servicesNodes(a string) []string {
	var out []string
	for _, n := range b.g.Nodes {
		if s := publisher(n); s != "" && (a == "*" || s == a) {
			out = append(out, n.ID)
		}
	}
	return out
}

// publisher is the app segment of the topics a node's service publishes.
func publisher(n *node) string {
	switch n.Kind {
	case "download":
		return "url-to-bucket"
	case "unzip":
		return "preprocessor"
	case "pipe":
		return "duckpipe"
	case "job":
		return "lakeshape"
	case "analyst":
		return "analyst"
	case "task":
		return "foreman"
	}
	return ""
}

// --- serving: semantic and mcp-hub ------------------------------------------

func (b *builder) serving(c *configs) {
	var semIDs []string
	for _, m := range c.Semantic {
		n := b.node("semantic:"+m.Name, "semantic", m.Name, "semantic", ptr(m.at))
		t := m.Table
		if !strings.Contains(t, ".") {
			t = "marts." + t
		}
		n.Sub = "on " + t
		mt := b.nodes["table:"+tableID(t)]
		if mt == nil || mt.Kind != "mart" {
			b.problem(m.at, "semantic model %s is on %s, which isn't a mart", m.Name, t)
		}
		b.edge(b.table(t, nil).ID, n.ID, "table", "")
		semIDs = append(semIDs, n.ID)
	}
	servers := map[string]bool{}
	for _, s := range c.Servers {
		n := b.node("server:"+s.Name, "server", s.Name, "mcp-hub", ptr(s.at))
		n.Sub = "MCP server"
		servers[s.Name] = true
		switch s.Name {
		case "semantic":
			for _, id := range semIDs {
				b.edge(id, n.ID, "tools", "")
			}
		case "vigil":
			n.Info = append(n.Info, "vigil reads every relay event")
		}
	}
	for _, p := range c.Profiles {
		n := b.node("profile:"+p.Name, "profile", p.Name, "mcp-hub", ptr(p.at))
		n.Sub = "mcp-hub profile"
		for _, s := range p.Servers {
			if !servers[s] {
				b.problem(p.at, "profile %s names server %s, which servers.json doesn't have", p.Name, s)
				continue
			}
			b.edge("server:"+s, n.ID, "tools", "")
		}
	}
}

// --- agents: foreman and analyst ---------------------------------------------

func (b *builder) agents(c *configs) {
	for _, a := range c.Agents {
		n := b.node("agent:"+a.Name, "agent", a.Name, "foreman", ptr(a.at))
		n.Sub = "agent, model " + a.Model
		b.useProfile(n, a.Profile, a.at)
	}
	if c.Analyst != nil {
		n := b.node("analyst", "analyst", "analyst", "analyst", c.Analyst)
		n.Sub = "chat over the semantic layer"
		b.useProfile(n, "analyst", *c.Analyst)
	}
	for _, t := range c.Tasks {
		n := b.node("task:"+t.Name, "task", t.Name, "foreman", ptr(t.at))
		n.Sub = "foreman task"
		if b.nodes["agent:"+t.Agent] == nil {
			b.problem(t.at, "task %s uses agent %s, which doesn't exist", t.Name, t.Agent)
		} else {
			b.edge("agent:"+t.Agent, n.ID, "tools", "runs")
		}
		if t.Every != "" {
			n.Info = append(n.Info, "every "+t.Every)
		}
		if t.OnEvent == "" {
			continue
		}
		n.Info = append(n.Info, "wakes on "+t.OnEvent)
		f := strings.Fields(t.OnEvent)
		topic, types := f[0], strings.Join(f[1:], ",")
		if !b.seen(topic) {
			b.problem(t.at, "task %s waits for %s, a topic relay has never seen", t.Name, topic)
		}
		from := b.servicesNodes(app(topic))
		if types == "failed" {
			// Every service would point at foreman: mark the nodes instead.
			for _, id := range from {
				if id != n.ID {
					b.nodes[id].OnFail = append(b.nodes[id].OnFail, t.Name)
				}
			}
			continue
		}
		for _, id := range from {
			if id != n.ID {
				e := b.edge(id, n.ID, "event", t.OnEvent)
				e.Live = b.lastMatching(trigger{Topic: topic, Type: types})
			}
		}
	}
}

func (b *builder) useProfile(n *node, name string, at loc) {
	if name == "" {
		b.problem(at, "%s has no profile", n.Label)
		return
	}
	if b.nodes["profile:"+name] == nil {
		b.problem(at, "%s uses profile %s, which servers.json doesn't have", n.Label, name)
		return
	}
	b.edge("profile:"+name, n.ID, "tools", "")
}

// --- live state --------------------------------------------------------------

func toLive(ev event) *live {
	l := &live{Topic: ev.Topic, Type: ev.Type, Time: ev.Time}
	if s, ok := ev.Data["error"].(string); ok {
		l.Error, _, _ = strings.Cut(s, "\n")
	}
	return l
}

// lastMatching returns the newest event a trigger matches: topic, type, and
// each data field by equality.
func (b *builder) lastMatching(t trigger) *live {
	for _, ev := range b.rc.Events {
		if !topicMatch(t.Topic, ev.Topic) || !typeMatch(t.Type, ev.Type) {
			continue
		}
		ok := true
		for k, v := range t.Data {
			if fmt.Sprint(ev.Data[k]) != fmt.Sprint(v) {
				ok = false
				break
			}
		}
		if ok {
			return toLive(ev)
		}
	}
	return nil
}

// live gives each node its newest event.
func (b *builder) live() {
	for _, n := range b.g.Nodes {
		for _, ev := range b.rc.Events {
			if ownEvent(n, ev) {
				n.Live = toLive(ev)
				break
			}
		}
	}
}

func ownEvent(n *node, ev event) bool {
	str := func(k string) string { s, _ := ev.Data[k].(string); return s }
	switch n.Kind {
	case "download":
		return ev.Topic == "url-to-bucket.download" && ev.Type != "requested"
	case "unzip":
		return strings.HasPrefix(ev.Topic, "preprocessor.") && (str("job") == n.Label || str("name") == n.Label)
	case "pipe":
		return strings.HasPrefix(ev.Topic, "duckpipe.") && str("pipe") == n.Label
	case "job":
		if ev.Topic == "lakeshape.trigger" {
			return str("job") == n.Label
		}
		if ev.Topic == "lakeshape.run" {
			jobs, _ := ev.Data["jobs"].([]any)
			for _, j := range jobs {
				if j == n.Label {
					return true
				}
			}
		}
	case "task":
		return ev.Topic == "foreman."+n.Label
	case "analyst":
		return strings.HasPrefix(ev.Topic, "analyst.")
	}
	return false
}

// --- layout ------------------------------------------------------------------

// toolsRank places MCP servers, profiles and agents after the data, in this
// order, even when nothing upstream pushes them there (vigil's server).
var toolsRank = map[string]int{"server": 0, "profile": 1, "agent": 2, "analyst": 2, "task": 3}

// layout puts each node one column right of its furthest upstream node, and
// orders each column by where its upstream nodes are, so lines cross less.
func (b *builder) layout() {
	preds := map[string][]string{}
	for _, e := range b.g.Edges {
		preds[e.To] = append(preds[e.To], e.From)
	}
	b.longestPath(preds)
	data := 0
	for _, n := range b.g.Nodes {
		if _, ok := toolsRank[n.Kind]; !ok {
			data = max(data, n.Col+1)
		}
	}
	for _, n := range b.g.Nodes {
		if r, ok := toolsRank[n.Kind]; ok {
			n.Col = max(n.Col, data+r)
		}
	}
	b.longestPath(preds)
	cols := map[int][]*node{}
	maxCol := 0
	for _, n := range b.g.Nodes {
		cols[n.Col] = append(cols[n.Col], n)
		maxCol = max(maxCol, n.Col)
	}
	for c := 0; c <= maxCol; c++ {
		ns := cols[c]
		key := map[string]float64{}
		for i, n := range ns {
			key[n.ID] = float64(i) // first seen order: the order of the config files
			if ps := preds[n.ID]; len(ps) > 0 {
				sum := 0.0
				for _, p := range ps {
					sum += float64(b.nodes[p].Row)
				}
				key[n.ID] = sum / float64(len(ps))
			}
		}
		sort.SliceStable(ns, func(i, j int) bool { return key[ns[i].ID] < key[ns[j].ID] })
		for i, n := range ns {
			n.Row = i
		}
	}
}

// longestPath moves each node right of all its upstream nodes. A cycle
// stops after len(nodes) passes.
func (b *builder) longestPath(preds map[string][]string) {
	for range b.g.Nodes {
		changed := false
		for _, n := range b.g.Nodes {
			for _, p := range preds[n.ID] {
				if c := b.nodes[p].Col + 1; c > n.Col && c < 2*len(b.g.Nodes) {
					n.Col, changed = c, true
				}
			}
		}
		if !changed {
			return
		}
	}
}
