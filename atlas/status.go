package main

// Services: whether each service's own page answers right now, checked from
// the server on 127.0.0.1. Any HTTP answer counts as up, including a login
// prompt (401) or a redirect; no answer within probeTimeout, or a 5xx, is
// down.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const probeTimeout = 2 * time.Second

// probeClient doesn't follow redirects: a redirect (e.g. to a login page)
// already means the service is up.
var probeClient = &http.Client{
	Timeout:       probeTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// status is one link's state, in GET /api/status.
type status struct {
	link
	Up    bool   `json:"up"`
	Ms    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// statuses probes every link in parallel.
func statuses(ctx context.Context, links []link) []status {
	res := make([]status, len(links))
	var wg sync.WaitGroup
	for i, l := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = probe(ctx, l)
		}()
	}
	wg.Wait()
	return res
}

func probe(ctx context.Context, l link) status {
	st := status{link: l}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", l.Port, l.Path), nil)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	start := time.Now()
	resp, err := probeClient.Do(req)
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
