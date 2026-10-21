package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func cliEnv(t *testing.T) {
	ts := newTestServer(t)
	t.Setenv("RELAY_URL", ts.URL)
	t.Setenv("RELAY_TOKEN", testToken)
}

func TestCLIPubTail(t *testing.T) {
	cliEnv(t)
	code, out, errOut := runTest(t, "pub", "a.download", "started", "-run-id", "r1", "-data", `{"url":"x"}`)
	if code != 0 || !strings.Contains(out, `"id":1`) {
		t.Fatalf("pub: %d %s %s", code, out, errOut)
	}
	runTest(t, "pub", "-run-id", "r1", "a.download", "ok") // flags before positionals
	code, out, _ = runTest(t, "tail", "-n", "5")
	if code != 0 || strings.Count(out, "\n") != 2 {
		t.Fatalf("tail: %d %q", code, out)
	}
	code, out, _ = runTest(t, "tail", "-after", "1")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, `"type":"ok"`) {
		t.Fatalf("tail -after: %d %q", code, out)
	}
	if code, _, errOut := runTest(t, "pub", "a.b"); code != exitUsage || errOut == "" {
		t.Fatalf("missing type: %d", code)
	}
	if code, _, errOut := runTest(t, "pub", "Bad", "ok"); code != exitErr || !strings.Contains(errOut, `"error"`) {
		t.Fatalf("bad topic: %d %s", code, errOut)
	}
}

func TestCLIWait(t *testing.T) {
	cliEnv(t)
	runTest(t, "pub", "a.download", "started", "-run-id", "r1")
	runTest(t, "pub", "a.download", "failed", "-run-id", "r1", "-data", `{"error":"boom"}`)

	// Looks back through the run.
	code, out, _ := runTest(t, "wait", "-run-id", "r1", "-type", "ok,failed", "-timeout", "1s")
	if code != exitFailed || !strings.Contains(out, "boom") {
		t.Fatalf("wait failed: %d %s", code, out)
	}
	code, _, _ = runTest(t, "wait", "-run-id", "r2", "-type", "ok,failed", "-timeout", "300ms")
	if code != exitNothing {
		t.Fatalf("wait timeout: %d", code)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		runTest(t, "pub", "a.download", "ok", "-run-id", "r3")
	}()
	code, out, _ = runTest(t, "wait", "-topic", "a.*", "-type", "ok", "-timeout", "5s")
	if code != exitOK || !strings.Contains(out, `"run_id":"r3"`) {
		t.Fatalf("wait ok: %d %s", code, out)
	}
}

func TestCLIConsumerNextAck(t *testing.T) {
	cliEnv(t)
	if code, _, errOut := runTest(t, "consumer", "idx", "-topic", "a.*", "-from", "earliest"); code != 0 {
		t.Fatalf("consumer: %d %s", code, errOut)
	}
	if code, _, _ := runTest(t, "next", "idx", "-wait", "0s"); code != exitNothing {
		t.Fatalf("next on empty: %d", code)
	}
	runTest(t, "pub", "a.b", "ok")
	code, out, _ := runTest(t, "next", "idx")
	if code != 0 || !strings.Contains(out, `"id":1`) {
		t.Fatalf("next: %d %s", code, out)
	}
	code, out, _ = runTest(t, "ack", "idx", "1")
	if code != 0 || !strings.Contains(out, `"cursor":1`) {
		t.Fatalf("ack: %d %s", code, out)
	}
	code, out, _ = runTest(t, "consumers")
	if code != 0 || !strings.Contains(out, `"backlog":0`) {
		t.Fatalf("consumers: %d %s", code, out)
	}
}

func TestCLIListen(t *testing.T) {
	cliEnv(t)
	listenBackoff = 50 * time.Millisecond
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	tries := filepath.Join(dir, "tries")

	runTest(t, "consumer", "worker", "-topic", "jobs.*")
	runTest(t, "pub", "jobs.x", "requested", "-run-id", "r1")
	runTest(t, "pub", "jobs.x", "requested", "-run-id", "r2")

	// Fails the first time it sees r1, then succeeds; records each handled event.
	script := `echo x >> "$TRIES"
if [ "$RELAY_RUN_ID" = r1 ] && [ "$(wc -l < "$TRIES")" -eq 1 ]; then exit 1; fi
cat >> "$SEEN"`
	t.Setenv("TRIES", tries)
	t.Setenv("SEEN", seen)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	var errOut bytes.Buffer
	go func() {
		done <- run(ctx, []string{"listen", "worker", "-wait", "200ms", "--", "sh", "-c", script}, nil, &bytes.Buffer{}, &errOut)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, out, _ := runTest(t, "consumers"); strings.Contains(out, `"cursor":2`) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("listen did not ack both events\n%s", errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("listen exit: %d", code)
	}

	b, _ := os.ReadFile(seen)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var first Event
	json.Unmarshal([]byte(lines[0]), &first)
	if first.RunID != "r1" {
		t.Fatalf("first handled event: %+v (order must be kept)", first)
	}
	if b, _ := os.ReadFile(tries); strings.Count(string(b), "\n") != 3 {
		t.Fatalf("tries = %d, want 3 (one retry)", strings.Count(string(b), "\n"))
	}
}
