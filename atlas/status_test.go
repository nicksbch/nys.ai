package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		code int
		up   bool
	}{{200, true}, {401, true}, {302, true}, {503, false}} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.code == 302 {
				w.Header().Set("Location", "/login")
			}
			w.WriteHeader(tc.code)
		}))
		if st := probe(context.Background(), link{"x", portOf(t, ts.URL), "/"}); st.Up != tc.up {
			t.Errorf("status %d: up = %v, want %v (%s)", tc.code, st.Up, tc.up, st.Error)
		}
		ts.Close()
	}

	// Nothing listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if st := probe(context.Background(), link{"x", port, "/"}); st.Up || st.Error != "no answer" {
		t.Errorf("closed port: %+v", st)
	}
}

// GET /api/status lists every link, in order, behind the password.
func TestStatusAPI(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	a := &atlas{links: []link{{"up", portOf(t, ts.URL), "/"}, {"down", 1, "/ui"}}}
	srv := httptest.NewServer(a.routes("u:p"))
	defer srv.Close()
	if resp, _ := http.Get(srv.URL + "/api/status"); resp.StatusCode != 401 {
		t.Errorf("no password: %s", resp.Status)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.SetBasicAuth("u", "p")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if s := string(body); !strings.Contains(s, `"name":"up","port":`) || !strings.Contains(s, `"up":true`) ||
		!strings.Contains(s, `"name":"down","port":1,"path":"/ui","up":false`) || strings.Index(s, `"up"`) > strings.Index(s, `"down"`) {
		t.Errorf("status = %s", s)
	}
}

func portOf(t *testing.T, url string) int {
	_, p, err := net.SplitHostPort(url[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}
