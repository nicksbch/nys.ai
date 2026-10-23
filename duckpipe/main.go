// duckpipe watches S3 prefixes (e.g. on the Garage set up by garage-setup.sh)
// and loads every new file into DuckDB tables, like Snowflake's Snowpipe. The
// tables live in a DuckLake: their data is Parquet in a bucket, and a small
// catalog (SQLite by default) records the tables and their snapshots.
//
// Each file is loaded exactly once. The list of loaded files is a table in the
// lake itself, written in the same transaction as the data, so a crash can never
// leave a file half-loaded or loaded twice. A file whose content changes (new
// ETag) is loaded again, as Snowpipe does.
//
// Credentials and endpoint come from the usual AWS env vars or ~/.aws profile:
//
//	AWS_ACCESS_KEY_ID=
//	AWS_SECRET_ACCESS_KEY=
//	AWS_DEFAULT_REGION=garage                  (default: garage)
//	AWS_ENDPOINT_URL=http://192.168.1.169:3900 (default)
//
// Usage:
//
//	duckpipe [-config duckpipe.yaml] run       # watch and load until stopped
//	duckpipe [-config duckpipe.yaml] once      # one pass over all pipes, then exit
//	duckpipe [-config duckpipe.yaml] status    # files and rows loaded, failures
//	duckpipe [-config duckpipe.yaml] retry [pipe]  # load failed files again
//	duckpipe [-config duckpipe.yaml] history [pipe] # every file loaded or failed, newest first
//	duckpipe [-config duckpipe.yaml] check     # check the config only; nothing is opened
//
// SIGHUP makes a running duckpipe poll immediately.
//
// "run" serves a web page on -listen (default 127.0.0.1:8100; empty for
// none): what is being loaded, the files waiting, the load history, and "Stop
// all", which stops the load under way and pauses polling until "Resume". Set
// -auth (or DUCKPIPE_UI_AUTH) to user:pass to require a login.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	fs := flag.NewFlagSet("duckpipe", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("DUCKPIPE_CONFIG", "duckpipe.yaml"), "config file")
	endpoint := fs.String("endpoint-url", envOr("AWS_ENDPOINT_URL", "http://192.168.1.169:3900"), "S3 endpoint")
	profile := fs.String("profile", "", "AWS profile name from ~/.aws")
	region := fs.String("region", "", "S3 region (default: from env/profile, else garage)")
	duckdbBin := fs.String("duckdb", envOr("DUCKPIPE_DUCKDB", "duckdb"), "duckdb CLI (1.5 or newer) to run loads with")
	stateDir := fs.String("state-dir", os.Getenv("DUCKPIPE_STATE_DIR"), "where DuckDB keeps extensions and spill files (default: DuckDB's own)")
	listen := fs.String("listen", "127.0.0.1:8100", "address of the web page (run only); empty for none")
	auth := fs.String("auth", os.Getenv("DUCKPIPE_UI_AUTH"), "require HTTP basic auth on the page, as user:pass")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Load new files from S3 into DuckLake tables, continuously.\n\n"+
			"usage: %s [flags] run | once | status | retry [pipe] | history [pipe] | check\n\n", fs.Name())
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
	if len(pos) == 0 || (pos[0] != "retry" && pos[0] != "history" && len(pos) > 1) || len(pos) > 2 {
		fs.Usage()
		os.Exit(2)
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := func() error {
		cfg, err := loadConfig(*cfgPath)
		if err != nil {
			return err
		}
		if pos[0] == "check" {
			fmt.Printf("%s: ok, %d pipes\n", *cfgPath, len(cfg.Pipes))
			return nil
		}
		e, err := open(ctx, cfg, *duckdbBin, *endpoint, *profile, *region, *stateDir)
		if err != nil {
			return err
		}

		switch pos[0] {
		case "run":
			if *listen != "" {
				go e.serve(ctx, *listen, *auth)
			}
			return e.run(ctx)
		case "once":
			if e.cycle(ctx) {
				return errors.New("some pipes could not be polled; see above")
			}
			return nil
		case "status":
			return e.status(ctx, os.Stdout)
		case "retry":
			pipe := ""
			if len(pos) == 2 {
				pipe = pos[1]
			}
			return e.retry(ctx, pipe)
		case "history":
			pipe := ""
			if len(pos) == 2 {
				pipe = pos[1]
			}
			return e.history(ctx, os.Stdout, pipe)
		}
		fs.Usage()
		os.Exit(2)
		return nil
	}()
	if err != nil {
		if ctx.Err() != nil {
			os.Exit(130)
		}
		log.Fatalf("error: %v", err)
	}
}

// run polls all pipes every cfg.Poll until ctx is cancelled. SIGHUP (or
// "Resume" on the page) starts the next poll right away. While stopped from
// the page, nothing is polled or loaded.
func (e *engine) run(ctx context.Context) error {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	log.Printf("watching %d pipe(s), polling every %s", len(e.cfg.Pipes), e.cfg.Poll.Duration)
	var lastMaint time.Time
	for {
		if wctx, done := e.work(ctx); wctx != nil {
			e.cycle(wctx)
			if m := e.cfg.Maintenance; m.Every.Duration > 0 && time.Since(lastMaint) >= m.Every.Duration && wctx.Err() == nil {
				e.setMaintaining(true)
				if err := e.maintain(wctx); err != nil {
					if wctx.Err() != nil {
						err = context.Cause(wctx)
					}
					log.Printf("maintenance: %v", err)
				}
				e.setMaintaining(false)
				// Maintenance stopped from the page runs again on the next poll.
				if wctx.Err() == nil {
					lastMaint = time.Now()
				}
			}
			done()
		}
		select {
		case <-ctx.Done():
			log.Printf("stopping")
			return nil
		case <-hup:
			log.Printf("SIGHUP: polling now")
		case <-e.poke:
		case <-time.After(e.cfg.Poll.Duration):
		}
	}
}
