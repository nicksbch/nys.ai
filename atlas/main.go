// atlas shows how the server's services are wired together: what writes
// where, what that wakes up, and which agent can use which tools. It reads
// the config files the services run with, works out the links between them,
// and draws them as one graph, with each node's last relay event. It is
// read-only: to act, follow a node's link to the service's own page. Its
// first tab is the server's homepage: a card per service's page, with
// whether it answers right now.
//
// Usage:
//
//	atlas serve [-listen 127.0.0.1:8101]   the page, behind ATLAS_AUTH (user:pass)
//	atlas check                            print the links that don't add up; exit 1 if any
//
// With -repo DIR it reads a checkout of this repo instead of the server's
// files: go run . -repo .. check
//
// Relay is read with RELAY_URL and RELAY_TOKEN. Without them the graph still
// draws, with no live state and no observed downloads.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const defaultLinks = "url-to-bucket=8093 preprocessor=8099 duckpipe=8100 garage-ui=8080 lake-ui=8092 foreman=8096/ui analyst=8098 nexus=8102 relay=8091/ui vigil=8090"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet("atlas "+cmd, flag.ExitOnError)
	repo := fs.String("repo", "", "read the configs from this checkout of the repo, not from the server's files")
	lakeshape := fs.String("lakeshape", "lakeshape", "lakeshape binary, run as `lakeshape ls` to see what each job builds")
	listen := fs.String("listen", "127.0.0.1:8101", "address to serve the page on (serve)")
	links := fs.String("links", defaultLinks, "the services' pages, as name=port[/path] separated by spaces: the Services cards and the graph's links (serve)")
	auth := fs.String("auth", os.Getenv("ATLAS_AUTH"), "user:pass for the page (serve)")
	fs.Parse(os.Args[2:])

	p := serverPaths
	if *repo != "" {
		p = repoPaths(*repo)
	}
	a := &atlas{
		paths: p,
		relay: &relayClient{url: os.Getenv("RELAY_URL"), token: os.Getenv("RELAY_TOKEN"), client: &http.Client{Timeout: 5 * time.Second}},
		ls:    lakeshapeLs(*lakeshape, p.Lakeshape),
	}

	switch cmd {
	case "check":
		os.Exit(a.check(context.Background()))
	case "serve":
		l, err := parseLinks(*links)
		if err != nil {
			log.Fatalf("-links: %v", err)
		}
		if !strings.Contains(*auth, ":") {
			log.Fatal("ATLAS_AUTH (or -auth) must be set as user:pass: the page is never served without a password")
		}
		a.links = l
		serve(a, *listen, *auth)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `atlas: the server's homepage, and how its services are wired together

usage:
  atlas serve [-listen 127.0.0.1:8101] [-links "name=port[/path] ..."]
  atlas check

flags (both):
  -repo DIR        read a checkout of the repo instead of the server's files
  -lakeshape BIN   lakeshape binary (default "lakeshape")

env: ATLAS_AUTH (user:pass, serve), RELAY_URL, RELAY_TOKEN
`)
}

func serve(a *atlas, listen, auth string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: listen, Handler: a.routes(auth), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("atlas on http://%s", listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// check prints every problem as file:line: text. 0 if there are none, 1 if
// there are.
func (a *atlas) check(ctx context.Context) int {
	g := a.graph(ctx)
	if g.Relay != "" {
		fmt.Fprintf(os.Stderr, "relay: %s (topics and downloads not checked)\n", g.Relay)
	}
	for _, p := range g.Problems {
		switch {
		case p.Line > 0:
			fmt.Printf("%s:%d: %s\n", p.File, p.Line, p.Text)
		case p.File != "":
			fmt.Printf("%s: %s\n", p.File, p.Text)
		default:
			fmt.Println(p.Text)
		}
	}
	if len(g.Problems) > 0 {
		return 1
	}
	fmt.Printf("ok: %d nodes, %d links, no problems\n", len(g.Nodes), len(g.Edges))
	return 0
}

// lakeshapeLs runs `lakeshape -project DIR -target prod ls -select ...` and
// reads its table: MODEL RELATION MATERIALIZED TAGS DEPENDS ON..., where an
// empty column is "-". It needs no database.
func lakeshapeLs(bin, project string) lsFunc {
	return func(sel, exclude string) ([]lsModel, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		args := []string{"-project", project, "-target", "prod"}
		if sel != "" {
			args = append(args, "-select", sel)
		}
		if exclude != "" {
			args = append(args, "-exclude", exclude)
		}
		var out, errb bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, append(args, "ls")...)
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			if msg := strings.TrimSpace(errb.String()); msg != "" {
				err = errors.New(lastLine(msg))
			}
			return nil, err
		}
		return parseLs(out.String()), nil
	}
}

func parseLs(s string) []lsModel {
	var out []lsModel
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] == "MODEL" {
			continue
		}
		m := lsModel{Relation: f[1]}
		if f[4] != "-" {
			m.Deps = f[4:]
		}
		out = append(out, m)
	}
	return out
}

func lastLine(s string) string {
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
