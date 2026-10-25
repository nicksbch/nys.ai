// lakeshape builds SQL models into a DuckLake, like dbt. Models are .sql files
// with Jinja templates; ref() and source() calls make the DAG they are built in.
// See SPEC.md.
//
// Usage:
//
//	lakeshape [-project .] compile [-select …]  # the rendered SQL of each model
//	lakeshape [-project .] ls [-select …]       # models in DAG order; -orphans: tables no model builds
//	lakeshape [-project .] run [-select …] [-exclude …] [-full-refresh] [-vars …]
//	lakeshape [-project .] test [-select …]     # run tests without building
//	lakeshape [-project .] history [model]      # from run_history, newest first
//	lakeshape [-project .] serve [-listen …]    # the service: runs jobs.yml's jobs on prod; MCP tools on -listen
//	lakeshape [-project .] trigger <job>        # ask the running service to run a job now
//
// The CLI works on the dev target: models go to dev_<user>__<schema>.
// compile and ls can show prod with -target prod.
//
// Credentials and endpoint come from the usual AWS env vars or ~/.aws profile,
// and PG* env vars for a Postgres catalog.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// exitError is an error with its own exit code.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

func main() {
	fs := flag.NewFlagSet("lakeshape", flag.ExitOnError)
	dir := fs.String("project", envOr("LAKESHAPE_PROJECT", "."), "project directory")
	targetName := fs.String("target", "dev", "dev or prod; only compile and ls can show prod")
	user := fs.String("dev-user", os.Getenv("USER"), "user part of dev schemas, dev_<user>__<schema>")
	varsJSON := fs.String("vars", "", `JSON object overriding project.yaml vars, e.g. '{"start_date": "2024-01-01"}'`)
	sel := fs.String("select", "", "models to use, e.g. 'stg_orders+ tag:daily' (default: all)")
	excl := fs.String("exclude", "", "models to leave out, same syntax as -select")
	fullRefresh := fs.Bool("full-refresh", false, "run, trigger: build incremental models from scratch")
	listen := fs.String("listen", "", "serve: address of the MCP tools (/mcp); empty for none")
	orphans := fs.Bool("orphans", false, "ls: list tables and views in the project's schemas that no model builds")
	var lo lakeOptions
	fs.StringVar(&lo.duckdb, "duckdb", envOr("LAKESHAPE_DUCKDB", "duckdb"), "duckdb CLI (1.5 or newer) to run SQL with")
	fs.StringVar(&lo.endpoint, "endpoint-url", envOr("AWS_ENDPOINT_URL", "http://192.168.1.169:3900"), "S3 endpoint")
	fs.StringVar(&lo.region, "region", "", "S3 region (default: from env/profile, else garage)")
	fs.StringVar(&lo.profile, "profile", "", "AWS profile name from ~/.aws")
	fs.StringVar(&lo.stateDir, "state-dir", os.Getenv("LAKESHAPE_STATE_DIR"), "where DuckDB keeps extensions and spill files (default: DuckDB's own)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Build SQL models into a DuckLake.\n\n"+
			"usage: %s [flags] compile | ls | run | test | history [model] | serve | trigger <job>\n\n", fs.Name())
		fs.PrintDefaults()
	}

	// Allow flags anywhere on the command line, not just before positionals.
	var pos []string
	for args := os.Args[1:]; ; {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) == 0 || len(pos) > 2 || (len(pos) == 2 && pos[0] != "history" && pos[0] != "trigger") || (len(pos) == 1 && pos[0] == "trigger") {
		fs.Usage()
		os.Exit(2)
	}
	log.SetFlags(0)
	if err := checkFlags(fs, pos[0]); err != nil {
		log.Printf("error: %v", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := func() error {
		switch pos[0] {
		case "serve": // always prod
			return serve(ctx, *dir, lo, os.Stdout, *listen, os.Getenv("LAKESHAPE_MCP_TOKEN"))
		case "trigger":
			p, err := loadProject(*dir, target{name: "prod"}, nil)
			if err != nil {
				return err
			}
			return triggerJob(ctx, p, pos[1], *fullRefresh, os.Stdout)
		}
		t := target{name: *targetName}
		switch t.name {
		case "dev":
			u, err := devUser(*user)
			if err != nil {
				return err
			}
			t.user = u
		case "prod":
			if pos[0] != "compile" && pos[0] != "ls" || *orphans {
				return fmt.Errorf("-target prod: the CLI only builds dev; prod is built by the service")
			}
		default:
			return fmt.Errorf("-target %q: use dev or prod", t.name)
		}
		var vars map[string]any
		if *varsJSON != "" {
			if err := json.Unmarshal([]byte(*varsJSON), &vars); err != nil {
				return fmt.Errorf("-vars: %w", err)
			}
		}
		p, err := loadProject(*dir, t, vars)
		if err != nil {
			return err
		}
		models, err := p.selectModels(*sel, *excl)
		if err != nil {
			return err
		}

		switch pos[0] {
		case "compile":
			return compileAll(p, models, os.Stdout)
		case "ls":
			if !*orphans {
				return list(p, models, os.Stdout)
			}
		case "run", "test", "history":
		default:
			fs.Usage()
			os.Exit(2)
		}

		l, err := openLake(ctx, p, lo)
		if err != nil {
			return &exitError{1, err}
		}
		var res *runResult
		switch pos[0] {
		case "ls":
			rels, err := l.orphans(ctx)
			for _, r := range rels {
				fmt.Println(r)
			}
			return err
		case "history":
			name := ""
			if len(pos) == 2 {
				name = pos[1]
			}
			return l.history(ctx, os.Stdout, name, 50)
		case "run":
			res, err = l.build(ctx, models, runOptions{fullRefresh: *fullRefresh, out: os.Stdout})
		case "test":
			res, err = l.test(ctx, models, runOptions{out: os.Stdout})
		}
		if err != nil {
			return &exitError{1, err}
		}
		if !res.ok() {
			return &exitError{1, nil}
		}
		return nil
	}()
	var ee *exitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.err == nil:
		os.Exit(ee.code)
	case errors.As(err, &ee):
		log.Printf("error: %v", ee.err)
		os.Exit(ee.code)
	default:
		log.Printf("error: %v", err)
		os.Exit(2)
	}
}

// commandFlags lists, per command, the flags it uses besides the ones every
// command takes (-project, -duckdb, the S3 settings, -state-dir).
var commandFlags = map[string][]string{
	"compile": {"target", "dev-user", "vars", "select", "exclude"},
	"ls":      {"target", "dev-user", "vars", "select", "exclude", "orphans"},
	"run":     {"target", "dev-user", "vars", "select", "exclude", "full-refresh"},
	"test":    {"target", "dev-user", "vars", "select", "exclude"},
	"history": {"target", "dev-user"},
	"serve":   {"listen"},
	"trigger": {"full-refresh"},
}

// checkFlags refuses a flag the command would ignore, such as -dev-user on
// trigger, which asks the service to run a job in prod.
func checkFlags(fs *flag.FlagSet, cmd string) error {
	allowed, ok := commandFlags[cmd]
	if !ok {
		return nil // unknown command: usage is printed later
	}
	var err error
	fs.Visit(func(f *flag.Flag) {
		if err != nil || slices.Contains(allowed, f.Name) || !commandSpecific(f.Name) {
			return
		}
		err = fmt.Errorf("-%s does not apply to %s", f.Name, cmd)
		if cmd == "trigger" || cmd == "serve" {
			err = fmt.Errorf("%w: it runs in prod, in the service; use run for dev", err)
		}
	})
	return err
}

func commandSpecific(name string) bool {
	for _, flags := range commandFlags {
		if slices.Contains(flags, name) {
			return true
		}
	}
	return false
}

// compileAll prints the SQL of the models, in DAG order, as a first build
// (is_incremental() is false).
func compileAll(p *project, models []*model, w io.Writer) error {
	for i, m := range models {
		sql, err := p.compile(m, false)
		if err != nil {
			return err
		}
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "-- %s: %s (%s)\n%s\n", p.relation(m), m.cfg.Materialized, m.path, strings.TrimSpace(sql))
	}
	return nil
}

// list prints the models in DAG order.
func list(p *project, models []*model, w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tRELATION\tMATERIALIZED\tTAGS\tDEPENDS ON")
	for _, m := range models {
		deps := append([]string{}, m.refs...)
		for _, s := range m.srcs {
			deps = append(deps, "source:"+s)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.name, p.relation(m), m.cfg.Materialized,
			dash(strings.Join(m.cfg.Tags, ",")), dash(strings.Join(deps, " ")))
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
