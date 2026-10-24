package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubLs answers `lakeshape ls` for the repo's analytics project.
func stubLs(sel, _ string) ([]lsModel, error) {
	var out []lsModel
	for _, src := range strings.Fields(sel) {
		switch src {
		case "source:uk.price_paid+":
			out = append(out,
				lsModel{Relation: "staging.stg_price_paid", Deps: []string{"source:uk.price_paid"}},
				lsModel{Relation: "marts.price_paid_monthly", Deps: []string{"stg_price_paid"}})
		default:
			t := strings.TrimSuffix(strings.TrimPrefix(src, "source:uk."), "+")
			out = append(out, lsModel{Relation: "staging.stg_" + t, Deps: []string{"source:uk." + t}})
		}
	}
	return out, nil
}

func hasEdge(g *graph, from, to, kind string) bool {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Kind == kind {
			return true
		}
	}
	return false
}

func nodeByID(g *graph, id string) *node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// The repo's own configs link up with no problems.
func TestRepoConfigs(t *testing.T) {
	now := time.Now()
	rc := &recent{
		Topics: []string{"url-to-bucket.download", "preprocessor.unzip", "duckpipe.ingest", "lakeshape.run", "foreman.duckpipe-failure"},
		Events: []event{
			{Time: now, Topic: "duckpipe.ingest", Type: "failed", Data: map[string]any{"pipe": "crime_outcomes", "table": "lake.uk.crime_outcomes", "error": "bad CSV\nline 2"}},
			{Time: now.Add(-time.Minute), Topic: "duckpipe.ingest", Type: "ok", Data: map[string]any{"pipe": "crime_street", "table": "lake.uk.crime_street"}},
		},
		Downs: []event{
			{Time: now, Data: map[string]any{"bucket": "home", "key": "uk/crime/2026-09.zip"}},
			{Time: now, Data: map[string]any{"bucket": "home", "key": "uk/price-paid/pp-monthly.csv"}},
			{Time: now.Add(-40 * 24 * time.Hour), Data: map[string]any{"bucket": "home", "key": "uk/price-paid/old.csv"}},
		},
	}
	g := build(readConfigs(repoPaths("..")), rc, stubLs, now)
	for _, p := range g.Problems {
		t.Errorf("problem: %s:%d: %s", p.File, p.Line, p.Text)
	}
	for _, e := range [][3]string{
		{"s3://home/uk/crime/", "unzip:uk_crime", "files"},
		{"unzip:uk_crime", "s3://home/uk/crime/extracted/", "files"},
		{"s3://home/uk/crime/extracted/", "pipe:crime_street", "files"},
		{"pipe:crime_street", "table:uk.crime_street", "table"},
		{"pipe:crime_street", "job:crime", "event"},
		{"table:uk.price_paid", "job:price_paid", "table"},
		{"job:price_paid", "table:marts.price_paid_monthly", "table"},
		{"table:marts.price_paid_monthly", "semantic:price_paid_monthly", "table"},
		{"semantic:price_paid_monthly", "server:semantic", "tools"},
		{"server:semantic", "profile:analyst", "tools"},
		{"profile:analyst", "analyst", "tools"},
		{"profile:oncall", "agent:oncall", "tools"},
		{"agent:oncall", "task:duckpipe-failure", "tools"},
		{"url-to-bucket", "s3://home/uk/crime/", "observed"},
		{"url-to-bucket", "s3://home/uk/price-paid/", "observed"},
	} {
		if !hasEdge(g, e[0], e[1], e[2]) {
			t.Errorf("no %s edge %s -> %s", e[2], e[0], e[1])
		}
	}
	for _, e := range g.Edges {
		if e.From == "url-to-bucket" && e.To == "s3://home/uk/price-paid/" && e.Label != "1 download, 30 days" {
			t.Errorf("observed label %q: the 40-day-old download should not count", e.Label)
		}
	}

	pipe := nodeByID(g, "pipe:crime_outcomes")
	if pipe.Live == nil || pipe.Live.Type != "failed" || pipe.Live.Error != "bad CSV" {
		t.Errorf("crime_outcomes live = %+v", pipe.Live)
	}
	if len(pipe.OnFail) != 1 || pipe.OnFail[0] != "duckpipe-failure" {
		t.Errorf("crime_outcomes on_fail = %v", pipe.OnFail)
	}
	if !strings.Contains(pipe.At.Block, "name: crime_outcomes") || strings.Contains(pipe.At.Block, "crime_stop_and_search") {
		t.Errorf("crime_outcomes block:\n%s", pipe.At.Block)
	}
	for _, e := range g.Edges {
		if e.From == "pipe:crime_street" && e.To == "job:crime" && (e.Live == nil || e.Live.Type != "ok") {
			t.Errorf("crime_street -> crime live = %+v", e.Live)
		}
	}

	// Agents come after the data, tasks last.
	if nodeByID(g, "server:vigil").Col <= nodeByID(g, "semantic:price_paid_monthly").Col {
		t.Error("vigil's server should be right of the semantic models")
	}
	if nodeByID(g, "task:duckpipe-failure").Col <= nodeByID(g, "agent:oncall").Col {
		t.Error("tasks should be right of agents")
	}
}

// Broken links show up as problems, with the file and line.
func TestProblems(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pre.yaml", "jobs:\n  - name: z\n    bucket: home\n    prefix: in/\n    to: out/\n")
	write("dp.yaml", "pipes:\n  - name: a\n    bucket: home\n    prefix: other/\n    table: raw.a\n")
	write("ls/jobs.yml", "jobs:\n  - name: j\n    select: x\n    on:\n      - topic: duckpipe.ingest\n        data: {table: lake.raw.missing}\n")
	write("ls/sources.yml", "sources:\n  - name: raw\n    tables: [a, b]\n")
	write("ls/project.yaml", "name: x\n")
	write("sem.yml", "m:\n  table: nope\n")
	write("hub.json", `{"servers": [{"name": "s", "url": "http://x"}], "profiles": {"p": {"servers": ["s", "ghost"]}}}`)
	write("fm/agents/ag.md", "---\nprofile: nothere\nmodel: local\n---\nhi\n")
	write("fm/tasks/t.md", "---\non_event: nobody.* failed\nagent: ghost\n---\nhi\n")
	write("broken.yaml", "pipes: [\n")

	p := paths{
		Preprocessor: filepath.Join(dir, "pre.yaml"), Duckpipe: filepath.Join(dir, "dp.yaml"),
		Lakeshape: filepath.Join(dir, "ls"), Semantic: filepath.Join(dir, "sem.yml"),
		MCPHub: filepath.Join(dir, "hub.json"), Foreman: filepath.Join(dir, "fm"), Analyst: filepath.Join(dir, "an"),
	}
	rc := &recent{Topics: []string{"duckpipe.ingest"}}
	g := build(readConfigs(p), rc, nil, time.Now())
	var got []string
	for _, pr := range g.Problems {
		got = append(got, filepath.Base(pr.File)+": "+pr.Text)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"pre.yaml: preprocessor job z writes to home/out/, which no pipe or job reads",
		"dp.yaml: pipe a reads home/other/, which no preprocessor job writes to",
		"sources.yml: sources.yml lists raw.b, which no pipe loads",
		"jobs.yml: job j waits for loads of lake.raw.missing, which no pipe loads",
		"sem.yml: semantic model m is on marts.nope, which isn't a mart",
		"hub.json: profile p names server ghost",
		"ag.md: ag uses profile nothere",
		"t.md: task t uses agent ghost, which doesn't exist",
		"t.md: task t waits for nobody.*, a topic relay has never seen",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing problem %q in:\n%s", want, all)
		}
	}

	p.Duckpipe = filepath.Join(dir, "broken.yaml")
	g = build(readConfigs(p), rc, nil, time.Now())
	if !strings.Contains(g.Problems[0].Text, "doesn't parse") {
		t.Errorf("first problem = %q, want a parse error", g.Problems[0].Text)
	}
}

// Without relay, topics and downloads aren't checked: no false problems.
func TestNoRelay(t *testing.T) {
	g := build(readConfigs(repoPaths("..")), &recent{Err: "RELAY_URL is not set"}, stubLs, time.Now())
	if len(g.Problems) != 0 {
		t.Errorf("problems without relay: %+v", g.Problems)
	}
	if g.Relay == "" {
		t.Error("relay error not reported")
	}
}

func TestMatching(t *testing.T) {
	for _, c := range []struct {
		filter, topic string
		want          bool
	}{
		{"duckpipe.*", "duckpipe.ingest", true},
		{"duckpipe.ingest", "duckpipe.ingest", true},
		{"duckpipe.*", "duckpipeX.ingest", false},
		{"*", "a.b", true},
		{"a.b, c.*", "c.d", true},
		{"a.b", "a.bc", false},
	} {
		if got := topicMatch(c.filter, c.topic); got != c.want {
			t.Errorf("topicMatch(%q, %q) = %v", c.filter, c.topic, got)
		}
	}
	for _, c := range []struct {
		pattern, rel string
		want         bool
	}{
		{"*-street.csv", "2026-01/2026-01-kent-street.csv", true},
		{"*.zip", "2026-04.zip", true},
		{"*/x-*.csv", "a/x-1.csv", true},
		{"*/x-*.csv", "x-1.csv", false},
		{"", "anything", true},
	} {
		if got := patternMatch(c.pattern, c.rel); got != c.want {
			t.Errorf("patternMatch(%q, %q) = %v", c.pattern, c.rel, got)
		}
	}
	if got := tableID("lake.uk.crime_street"); got != "uk.crime_street" {
		t.Errorf("tableID = %q", got)
	}
}

func TestParseLs(t *testing.T) {
	out := `MODEL               RELATION                    MATERIALIZED  TAGS     DEPENDS ON
stg_price_paid      staging.stg_price_paid      view          staging  source:uk.price_paid
price_paid_monthly  marts.price_paid_monthly    table         -        stg_price_paid source:uk.x
lonely              staging.lonely              view          -        -
`
	ms := parseLs(out)
	if len(ms) != 3 || ms[1].Relation != "marts.price_paid_monthly" || len(ms[1].Deps) != 2 || ms[2].Deps != nil {
		t.Errorf("parseLs = %+v", ms)
	}
}

func TestAuth(t *testing.T) {
	a := &atlas{paths: repoPaths(".."), relay: &relayClient{}, ls: stubLs}
	h := a.routes("u:p")
	for _, c := range []struct {
		path, user, pass string
		want             int
	}{
		{"/healthz", "", "", 200},
		{"/", "", "", 401},
		{"/api/graph", "u", "wrong", 401},
		{"/api/graph", "u", "p", 200},
		{"/", "u", "p", 200},
	} {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		if c.user != "" {
			req.SetBasicAuth(c.user, c.pass)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s as %q: %d, want %d", c.path, c.user, rec.Code, c.want)
		}
	}
}
