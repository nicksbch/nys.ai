// relay is a small event log with subscriptions. Applications publish events
// ("url-to-bucket download started", "… ok"); other services and agents read
// them live, wait for a particular one, or pick up where they left off
// through a named consumer whose cursor the server keeps.
//
// The same binary is the server and the CLI:
//
//	relay serve -listen 127.0.0.1:8091 -data ./data
//	relay pub url-to-bucket.download started -run-id abc -data '{"url":"https://..."}'
//	relay wait -run-id abc -type ok,failed -timeout 30m
//	relay listen indexer -- ./index-new-file.sh
//
// GET / on the server, or "relay help", prints the full usage (usage.txt).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches a subcommand and returns the exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	c := &cli{ctx: ctx, stdin: stdin, stdout: stdout, stderr: stderr}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "pub":
		return c.pub(args[1:])
	case "tail":
		return c.tail(args[1:])
	case "wait":
		return c.wait(args[1:])
	case "topics":
		return c.topics(args[1:])
	case "consumer":
		return c.consumer(args[1:])
	case "consumers":
		return c.consumers(args[1:])
	case "next":
		return c.next(args[1:])
	case "ack":
		return c.ack(args[1:])
	case "listen":
		return c.listen(args[1:])
	}
	fmt.Fprintf(stderr, "relay: unknown command %q (try relay help)\n", args[0])
	return exitUsage
}

func serve(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("relay serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8091", "address to serve on")
	dataDir := fs.String("data", envOr("STATE_DIRECTORY", "./data"), "directory for the event log and consumers")
	retention := fs.Duration("retention", 30*24*time.Hour, "how long to keep events")
	token := fs.String("token", os.Getenv("RELAY_TOKEN"), "bearer token required on /v1 (default $RELAY_TOKEN)")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return exitUsage
	}

	st, err := openStore(*dataDir, *retention)
	if err != nil {
		log.Printf("event log: %v", err)
		return exitErr
	}
	defer st.close()
	cons, err := openConsumers(filepath.Join(*dataDir, "consumers.json"))
	if err != nil {
		log.Printf("consumers: %v", err)
		return exitErr
	}
	if *token == "" {
		log.Printf("warning: no -token set; anyone who can reach %s can publish and read", *listen)
	}

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				st.expire(now)
			}
		}
	}()

	s := &server{token: *token, store: st, consumers: cons}
	srv := &http.Server{Addr: *listen, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("relay listening on http://%s (data: %s, retention: %s, last id: %d)", *listen, *dataDir, *retention, st.latest())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
		return exitErr
	}
	return exitOK
}
