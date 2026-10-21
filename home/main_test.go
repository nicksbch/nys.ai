package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestParseServices(t *testing.T) {
	got, err := parseServices("vigil=8090 relay=8091/ui,lake=8092/a/b")
	if err != nil {
		t.Fatal(err)
	}
	want := []service{{"vigil", 8090, "/"}, {"relay", 8091, "/ui"}, {"lake", 8092, "/a/b"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got[i], want[i])
		}
	}
	for _, bad := range []string{"", "vigil", "=8090", "vigil=x", "vigil=0", "vigil=70000"} {
		if _, err := parseServices(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestProbe(t *testing.T) {
	s := &server{client: &http.Client{
		Timeout:       time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
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
		if st := s.probe(context.Background(), service{"x", portOf(t, ts.URL), "/"}); st.Up != tc.up {
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
	if st := s.probe(context.Background(), service{"x", port, "/"}); st.Up {
		t.Error("closed port reported up")
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
