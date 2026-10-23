// preprocessor watches S3 prefixes (e.g. on the Garage set up by
// garage-setup.sh) and unzips every new archive into another prefix of the
// same bucket, so loaders like duckpipe only ever see plain files.
//
// Entries are written under their own path in the archive, so archives that
// overlap (each month's download repeating the months before) write the same
// keys. An entry already in the bucket with the same CRC-32 and size is
// skipped, so it isn't rewritten and its ETag doesn't change.
//
// Each archive is unzipped once per ETag. What was unzipped or failed is kept
// in <state-dir>/state.json; a failed archive is only tried again after
// "preprocessor retry".
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
//	preprocessor [-config preprocessor.yaml] run           # watch and unzip until stopped, with a web page
//	preprocessor [-config preprocessor.yaml] once          # one pass over all jobs, then exit
//	preprocessor [-config preprocessor.yaml] status        # archives unzipped, failures
//	preprocessor [-config preprocessor.yaml] retry [job]   # try failed archives again
//	preprocessor [-config preprocessor.yaml] history [job] # every archive unzipped or failed, newest first
//	preprocessor [-config preprocessor.yaml] check         # check the config only; nothing is opened
//
// SIGHUP makes a running preprocessor poll immediately.
//
// "run" serves a web page on -listen (default 127.0.0.1:8099; empty for
// none): the jobs, their archives and history, with "Poll now", "Extract
// again" and removing records of past attempts. Set -auth (or
// PREPROCESSOR_UI_AUTH=user:pass) before exposing it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	relay "relay/client"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	fs := flag.NewFlagSet("preprocessor", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("PREPROCESSOR_CONFIG", "preprocessor.yaml"), "config file")
	endpoint := fs.String("endpoint-url", envOr("AWS_ENDPOINT_URL", "http://192.168.1.169:3900"), "S3 endpoint")
	profile := fs.String("profile", "", "AWS profile name from ~/.aws")
	region := fs.String("region", "", "S3 region (default: from env/profile, else garage)")
	stateDir := fs.String("state-dir", envOr("PREPROCESSOR_STATE_DIR", "."), "where state.json is kept and archives are downloaded to")
	listen := fs.String("listen", "127.0.0.1:8099", "address of the web page (run only); empty for none")
	auth := fs.String("auth", os.Getenv("PREPROCESSOR_UI_AUTH"), "require HTTP basic auth on the page, as user:pass")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Unzip new archives in S3 into another prefix, continuously.\n\n"+
			"usage: %s [flags] run | once | status | retry [job] | history [job] | check\n\n", fs.Name())
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
		e := &engine{
			cfg:     cfg,
			state:   &store{file: filepath.Join(*stateDir, "state.json")},
			workDir: filepath.Join(*stateDir, "tmp"),
			bus:     relay.FromEnv(),
			down:    map[string]bool{},
			poke:    make(chan struct{}, 1),
			listed:  map[string]listing{},
		}
		arg := ""
		if len(pos) == 2 {
			arg = pos[1]
		}
		switch pos[0] {
		case "check":
			fmt.Printf("%s: ok, %d jobs\n", *cfgPath, len(cfg.Jobs))
			return nil
		case "status":
			return e.status(os.Stdout)
		case "history":
			return e.history(os.Stdout, arg)
		case "retry":
			return e.retry(arg)
		case "run", "once":
		default:
			fs.Usage()
			os.Exit(2)
		}

		if e.s3, err = newS3(ctx, *endpoint, *profile, *region); err != nil {
			return err
		}
		e.up = manager.NewUploader(e.s3, func(u *manager.Uploader) { u.PartSize = chunk })
		if err := os.MkdirAll(e.workDir, 0o750); err != nil {
			return err
		}
		e.cleanWorkDir()
		if pos[0] == "once" {
			if e.cycle(ctx) {
				return errors.New("some jobs could not be polled; see above")
			}
			return nil
		}
		if *listen != "" {
			go e.serve(ctx, *listen, *auth)
		}
		return e.run(ctx)
	}()
	if err != nil {
		if ctx.Err() != nil {
			os.Exit(130)
		}
		log.Fatalf("error: %v", err)
	}
}

// run polls all jobs every cfg.Poll until ctx is cancelled. SIGHUP, or "Poll
// now" on the page, starts the next poll right away.
func (e *engine) run(ctx context.Context) error {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	log.Printf("watching %d job(s), polling every %s", len(e.cfg.Jobs), e.cfg.Poll.Duration)
	for {
		e.cycle(ctx)
		select {
		case <-ctx.Done():
			log.Printf("stopping")
			return nil
		case <-hup:
			log.Printf("SIGHUP: polling now")
		case <-e.poke:
			log.Printf("page: polling now")
		case <-time.After(e.cfg.Poll.Duration):
		}
	}
}
