// mcp-hub is one MCP endpoint for every service on the server. It reads a
// list of upstream MCP servers (servers.json), joins their tools into one
// catalogue named <server>__<tool>, and forwards each call to the server
// that owns it. Profiles (/mcp/<profile>) show an agent only some of the
// tools. Every call is published to relay as mcp-hub.call. See SPEC.md.
//
// Usage:
//
//	MCP_HUB_TOKEN=dev mcp-hub serve -listen 127.0.0.1:8094 -config ./servers.json
//	mcp-hub check -config ./servers.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	relay "relay/client"
)

const defaultConfig = "/etc/mcp-hub/servers.json"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "check":
		os.Exit(check(os.Args[2:]))
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8094", "address to serve on")
	path := fs.String("config", defaultConfig, "servers.json")
	fs.Parse(args)

	token := os.Getenv("MCP_HUB_TOKEN")
	if token == "" {
		log.Fatal("MCP_HUB_TOKEN is not set: the hub never serves tools without a token")
	}
	cfg, err := loadConfig(*path, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	h := newHub(cfg, token, relay.FromEnv())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h.run(ctx)

	srv := &http.Server{Addr: *listen, Handler: h.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("mcp-hub listening on http://%s (%d servers, %d profiles)", *listen, len(cfg.Servers), len(cfg.Profiles))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// check validates the config and connects to each upstream once, printing
// one JSON line per server. Exit codes: 0 all up, 1 some down, 2 bad config.
func check(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := fs.String("config", defaultConfig, "servers.json")
	fs.Parse(args)

	cfg, err := loadConfig(*path, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ups := make([]*upstream, len(cfg.Servers))
	var wg sync.WaitGroup
	for i, s := range cfg.Servers {
		ups[i] = newUpstream(s, func(*upstream, []*mcp.Tool) {})
		wg.Add(1)
		go func() {
			defer wg.Done()
			ups[i].sync(context.Background())
			ups[i].closeSession()
		}()
	}
	wg.Wait()

	rc, enc := 0, json.NewEncoder(os.Stdout)
	for _, u := range ups {
		st := u.status()
		enc.Encode(struct {
			Name  string `json:"name"`
			State string `json:"state"`
			Tools int    `json:"tools"`
			Error string `json:"error,omitempty"`
		}{st.Name, st.State, st.Tools, st.LastError})
		if st.State != "up" {
			rc = 1
		}
	}
	return rc
}
