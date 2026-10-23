// A small homepage for the server's web UIs: one link per service, with a
// dot showing whether it answers right now.
//
// Each service is given as name=port[/path]. Links point at the host the
// page was opened on, so the page works whatever address you reach it by.
// Status is checked from the server itself, on 127.0.0.1.
//
// Usage:
//
//	go run .                       # then open http://127.0.0.1:8000
//	go build -o home . && ./home -listen :8000 -services "vigil=8090 relay=8091/ui"
//
// The page only holds links and up/down states, so it needs no login; each
// service still asks for its own.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

const defaultServices = "vigil=8090 relay=8091/ui garage-ui=8080 lake-ui=8092 url-to-bucket=8093 preprocessor=8099 duckpipe=8100 foreman=8096/ui analyst=8098"

type service struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Path string `json:"path"`
}

// parseServices reads "name=port[/path] ..." separated by spaces or commas.
func parseServices(s string) ([]service, error) {
	var out []service
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' }) {
		name, rest, ok := strings.Cut(f, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("%q: want name=port[/path]", f)
		}
		port, path, _ := strings.Cut(rest, "/")
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%q: bad port %q", f, port)
		}
		out = append(out, service{name, n, "/" + path})
	}
	if len(out) == 0 {
		return nil, errors.New("no services")
	}
	return out, nil
}

type server struct {
	services []service
	client   *http.Client
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8000", "address to serve the page on")
	list := flag.String("services", defaultServices, "services to show, as name=port[/path] separated by spaces")
	flag.Parse()

	services, err := parseServices(*list)
	if err != nil {
		log.Fatalf("-services: %v", err)
	}
	s := &server{
		services: services,
		client: &http.Client{
			Timeout: 2 * time.Second,
			// A redirect (e.g. to a login page) already means it is up.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{
		Addr:              *listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("home on http://%s (%d services)", *listen, len(services))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

type state struct {
	service
	Up    bool   `json:"up"`
	Ms    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// status probes every service in parallel. Any HTTP answer counts as up,
// including 401: the service is there, it just wants a login.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	res := make([]state, len(s.services))
	var wg sync.WaitGroup
	for i, svc := range s.services {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = s.probe(r.Context(), svc)
		}()
	}
	wg.Wait()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(res)
}

func (s *server) probe(ctx context.Context, svc service) state {
	st := state{service: svc}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", svc.Port, svc.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	start := time.Now()
	resp, err := s.client.Do(req)
	st.Ms = time.Since(start).Milliseconds()
	if err != nil {
		st.Error = "no answer"
		return st
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		st.Error = resp.Status
		return st
	}
	st.Up = true
	return st
}
