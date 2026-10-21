# vigil

A small monitor for the server and the applications on it. It is a single Go
binary that needs nothing else on the server and has the dashboard built in.

- **System:** disk space for each path you list, plus CPU, memory and load. It
  samples them every 15 s and keeps 24 hours of history in memory.
- **Applications:** your apps report each download, ingestion or other job as
  an **event**, by publishing it to [relay](../relay) (vigil reads it from
  there) or by posting it to vigil directly. An event has a status (`ok` or `failed`), a size, an item count
  and a duration. A failed event also carries the error detail as text. Events
  are kept for 90 days in `events.jsonl`, and the last 7 days also in
  memory for the MCP tools.
- **Alerts:** an alert fires when a disk is too full, when CPU or memory stays
  high, when a job fails, or when a job hasn't succeeded for longer than
  expected ("no ingestion in 24 h"). Alerts show on the dashboard, go to the
  journal, and can be posted to a webhook (Slack, Mattermost, Discord, or
  anything that accepts JSON). Each condition is announced when it starts and
  again when it clears, not on every check.
- **Dashboard:** current values, 24-hour charts, disk meters, per-app totals and
  the latest events, with the full error text of failures one click away.
- **MCP:** the same data as six read-only tools at `/mcp`, so an AI agent can
  ask what's wrong, what ran and why it failed (see [MCP](#mcp) below).

## Files

| File               | What it is                                                          |
|--------------------|---------------------------------------------------------------------|
| `main.go`          | Flags, sampling loop, alert checks, HTTP API                        |
| `system.go`        | Disk (`statfs`), CPU, memory and load (`/proc`)                     |
| `events.go`        | The event log (append-only JSON lines) and per-app aggregates       |
| `alerts.go`        | Alert state and the webhook                                         |
| `relay.go`         | Reads application events from relay                                 |
| `mcp.go`           | The MCP tools at `/mcp` ([MCP.md](MCP.md) is their spec)            |
| `index.html`       | The dashboard (plain HTML and JavaScript, built into the binary)    |
| `client/client.go` | Go package your apps import to report events                        |
| `integration/`     | Patch that adds reporting to garage-ui                              |
| `deploy.sh`        | Run on your Mac: builds the binary and installs it on the server    |
| `install.sh`       | Run on the server by `deploy.sh`: binary, secrets, systemd service  |

## Run it locally

```sh
go build -o vigil . && ./vigil -disks / -data ./data   # then open http://127.0.0.1:8090
```

macOS has no `/proc`, so locally you only get disks and events. CPU and memory
appear on Linux.

| Flag          | Default            | Meaning                                                        |
|---------------|--------------------|----------------------------------------------------------------|
| `-listen`     | `127.0.0.1:8090`   | Address to serve on                                            |
| `-disks`      | `/`                | Comma-separated paths whose filesystems to watch               |
| `-data`       | `$STATE_DIRECTORY` or `./data` | Where `events.jsonl` lives                         |
| `-auth`       | `$VIGIL_AUTH`     | `user:pass` required for the dashboard and `/api/status`       |
| `-token`      | `$VIGIL_TOKEN`    | Bearer token required to post events                           |
| `-mcp-token`  | `$VIGIL_MCP_TOKEN` | Bearer token for `/mcp`. Empty: `/mcp` is not served          |
| `-webhook`    | `$VIGIL_WEBHOOK`  | URL to POST alerts to                                          |
| `-expect`     |                    | `app/kind=24h`: alert if no success for that long (repeatable) |
| `-disk-warn`  | `85`               | Disk % full that triggers an alert                             |
| `-cpu-warn`   | `90`               | CPU % that triggers an alert once sustained                    |
| `-mem-warn`   | `90`               | Memory % that triggers an alert once sustained                 |
| `-sustain`    | `5m`               | How long CPU/memory must stay high                             |
| `-interval`   | `15s`              | Sampling interval                                              |
| `-retention`  | `2160h` (90 days)  | How long events are kept                                       |
| `-recent`     | `168h` (7 days)    | How long events stay in memory for the MCP tools (at least `24h`; at most 200,000 events) |
| `-relay`      | `$RELAY_URL`       | Read application events from this relay (empty: don't)         |
| `-relay-token`| `$RELAY_TOKEN`     | Bearer token for relay                                         |

## Deploy to the server

```sh
./deploy.sh
# with settings:
EXPECT="url-to-bucket/download=24h garage-ui/ingest=168h" \
WEBHOOK=https://hooks.slack.com/services/... \
DISKS="/ /var/lib/garage" ./deploy.sh
```

| Variable     | Default               | Meaning                                                          |
|--------------|-----------------------|------------------------------------------------------------------|
| `HOST`       | `192.168.1.169`       | SSH destination                                                  |
| `SSH_PORT`   | `2222`                | SSH port on the server                                           |
| `LISTEN`     | `[::]:8090`           | Where vigil listens. `127.0.0.1:8090` keeps it local            |
| `DISKS`      | `/ /var/lib/garage`   | Paths to watch, separated by spaces. Missing ones are skipped    |
| `EXPECT`     |                       | `app/kind=duration` entries, separated by spaces                 |
| `WEBHOOK`    |                       | Alert webhook (only used the first time; later edit `/etc/vigil.env`) |
| `UI_USER`    | `admin`               | Dashboard user name                                              |
| `EXTRA_ARGS` |                       | Any other flags, e.g. `-disk-warn 90 -cpu-warn 95`               |

The first run writes two files. Later runs leave them alone:

- `/etc/vigil.env` (root only): the dashboard login, the event token, the
  webhook and the MCP token. The MCP token is also added to an existing file
  that lacks it.
- `/etc/vigil-client.env` (readable by everyone): `VIGIL_URL` and
  `VIGIL_TOKEN`. This is all an application needs to report events. The token
  only allows posting events.

The service runs as a throwaway user (`DynamicUser=yes`) with the same
hardening as `garage-ui`. It keeps its event log in `/var/lib/vigil`. It also
reads `/etc/relay-client.env` if relay is installed, and then picks up
application events from relay. Deploy relay first (`deploy-all.sh` does).

**Note:** `DISKS`, `EXPECT` and `EXTRA_ARGS` are written into the systemd unit
on every deploy. Pass the full set each time, like `BUCKETS` for garage-ui.

After deploying:

- **Dashboard:** `http://192.168.1.169:8090` (login: `sudo grep VIGIL_AUTH /etc/vigil.env`)
- **Logs:** `journalctl -u vigil -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8090/tcp`

---

# Integrating your applications

## Through relay (preferred)

Publish to relay with topic `<app>.<kind>`, type `started`, `ok` or
`failed`, the same `run_id` on a run's events, and any of `name`, `bytes`,
`items`, `duration_ms` and `error` in `data`:

```sh
relay pub nightly.ingest failed -run-id abc -data '{"name":"orders.csv","error":"…"}'
```

vigil reads every such event through its relay consumer `vigil`, and treats
it exactly like one posted below. Other types are ignored. Events vigil has
already stored (by relay ID) are skipped, so redelivery is harmless.
**url-to-bucket** and **duckpipe** report this way.

## Directly

Without relay, a job can send one HTTP request when it finishes.

```http
POST /api/events
Authorization: Bearer $VIGIL_TOKEN
Content-Type: application/json

{"app":"url-to-bucket","kind":"download","status":"ok",
 "name":"https://…/file.iso","bytes":1500000000,"items":1,"duration_ms":42000}
```

A failure has `"status":"failed"` and the detail in `error`, as plain text up to
16 KB (a message, command output, a stack trace):

```json
{"app":"url-to-bucket","kind":"download","status":"failed","name":"https://…/file.iso",
 "duration_ms":1200,"error":"GET https://…/file.iso: 503 Service Unavailable"}
```

- `app` and `kind` are required: 1–64 characters of `A-Z a-z 0-9 _ . -`.
  Use your own kinds, for example `download`, `ingest`, `export` or `backup`.
- `status` is `ok` or `failed` when a job ends. If it is left out, an event
  with an `error` is `failed` and one without is `ok`. `error` is only accepted
  with `failed`.
- Optionally, send `"status":"started"` when a job begins, with the same `app`,
  `kind` and `name`. It appears in the recent events but does not count as a
  run, so totals and the "expected every" check only use `ok` and `failed`.
- `run_id` (optional, 1–64 characters of `A-Z a-z 0-9 _ . -`) links a `started`
  event to its result: send the same value on both. The dashboard then shows,
  on the `started` row, how the run ended and how long it took, or
  "no result yet" with the time since it started. That flags a job that crashed
  or hung before it could report. The Go client sets it for you.
- `name`, `bytes`, `items`, `duration_ms` and `time` are optional.
- A failed event raises an alert with the first line of `error`, sent at most
  once every 15 minutes per app/kind. The dashboard shows the full text.
- The response is `204` on success.

Two rules keep monitoring from ever hurting an app:

1. Use a short timeout (2–3 s).
2. Ignore errors from the report. A monitor that is down must never fail a
   download.

The Go client does both for you.

## Go apps: the `vigil/client` package

For **garage-ui**, a ready-made
patch is in `integration/`. Apply it from the `infra` directory, then add the
module:

```sh
patch -p1 < vigil/integration/garage-ui.patch
(cd garage-ui && go mod edit -require=vigil@v0.0.0 -replace=vigil=../vigil)
```

The patch contains the code shown below. To do it by hand, or for other Go
apps:

Add the dependency. The `replace` line points at this folder, so nothing needs
to be published. Run this from the app's directory:

```sh
go mod edit -require=vigil@v0.0.0 -replace=vigil=../vigil
```

Then use the client. Import it under the alias `vigil`, because many programs
already have a variable called `client` (garage-ui's S3 client is one):

```go
import vigil "vigil/client"

mon := vigil.FromEnv("my-app")   // reads VIGIL_URL, VIGIL_TOKEN; nil (no-op) if unset

op := mon.Track("ingest", "orders.csv")
op.Start()                        // optional "started" event (op.StartWait(ctx) in a CLI);
                                  // both events carry the same op.RunID()
r = op.Reader(r)                  // counts bytes as they're read (or op.Writer(w), op.AddBytes(n))
op.AddItems(rows)                 // optional: rows, files, records...
op.Done(err)                      // server: sends in the background
op.DoneWait(ctx, err)             // CLI: waits up to 3 s so the event isn't lost at exit
```

`err == nil` reports status `ok`. A non-nil `err` reports `failed`, with
`err.Error()` as the detail. To send more detail than the error message, such
as a command's output, send the event yourself:

```go
mon.Go(vigil.Event{Kind: "ingest", Name: "orders.csv", Status: vigil.Failed, Error: string(output)})
```

`FromEnv` takes `VIGIL_URL` and `VIGIL_TOKEN` from the environment. If they
aren't set, it reads them from `/etc/vigil-client.env`, so apps on the server
report from any shell, cron job or service without extra setup. If neither is
there, `FromEnv` returns `nil` and every call does nothing. So the integrated
apps still build, run and test the same way on your Mac.

### garage-ui (server): `ingest` for uploads, `download` for downloads

Add a `mon *vigil.Client` field to `server`, and set it in `main` with
`mon: vigil.FromEnv("garage-ui")`. Then wrap the two body streams:

```go
// upload
op := s.mon.Track("ingest", q.Get("bucket")+"/"+key)
_, err := s.uploader.Upload(r.Context(), &s3.PutObjectInput{
	// ...
	Body: op.Reader(r.Body),
	// ...
})
op.Done(err)

// download: only full downloads; video seeking sends many Range requests
w.WriteHeader(code)
if in.Range == nil {
	op := s.mon.Track("download", q.Get("bucket")+"/"+key)
	_, err := io.Copy(w, op.Reader(out.Body))
	op.Done(err)
} else {
	io.Copy(w, out.Body)
}
```

Then give the service the settings. In `garage-ui/install.sh`, add this line to
the `[Service]` section of the unit. The leading `-` means "fine if missing":

```ini
EnvironmentFile=-/etc/vigil-client.env
```

Redeploy garage-ui with `./deploy.sh`.

## Shell scripts and cron jobs

```sh
. /etc/vigil-client.env
report() {   # report <status> [error]; fields shared by the run's events
  curl -fsS -m 3 -o /dev/null -X POST "$VIGIL_URL/api/events" \
    -H "Authorization: Bearer $VIGIL_TOKEN" -H "Content-Type: application/json" \
    -d "$(jq -n --arg s "$1" --arg e "${2:-}" --arg id "$run_id" --argjson ms $(( $(date +%s%3N) - start )) \
          '{app:"nightly-ingest", kind:"ingest", status:$s, run_id:$id}
           + (if $s == "started" then {} else {duration_ms:$ms} end)
           + (if $e != "" then {error:$e} else {} end)')" || true
}

run_id=$(openssl rand -hex 8) start=$(date +%s%3N)   # %3N (milliseconds) needs GNU date, as on Linux
report started
if out=$(/usr/local/bin/nightly-ingest 2>&1); then
  report ok
else
  report failed "$(printf %s "$out" | tail -c 8000)"   # last part of the output
fi
```

`jq` builds the JSON safely, since error text can contain quotes. Without it,
send a fixed message instead.

## Python (e.g. dbt/DuckDB ingestion jobs)

```python
import json, os, time, urllib.request

def vigil_report(app, kind, status, run_id="", name="", bytes=0, items=0, duration_s=0.0, error=""):
    """status: "started", "ok" or "failed"; error: the failure detail as text
    (e.g. traceback.format_exc()); run_id: the same value (e.g. uuid.uuid4().hex)
    on a run's "started" event and its result."""
    url = os.environ.get("VIGIL_URL")
    if not url:
        return
    event = {"app": app, "kind": kind, "status": status, "name": name,
             "bytes": bytes, "items": items, "duration_ms": int(duration_s * 1000)}
    if run_id:
        event["run_id"] = run_id
    if error:
        event["error"] = error
    body = json.dumps(event).encode()
    req = urllib.request.Request(url + "/api/events", body, {
        "Content-Type": "application/json",
        "Authorization": "Bearer " + os.environ.get("VIGIL_TOKEN", "")})
    try:
        urllib.request.urlopen(req, timeout=3)
    except Exception:
        pass  # monitoring must never break the job
```

## Reading the data elsewhere

- `GET /api/status` (basic auth): everything the dashboard shows, as JSON.
- `GET /healthz`: no auth, for uptime checks.
- `/mcp` (MCP token): the tools below.

## MCP

vigil serves six tools over MCP (Streamable HTTP) at `/mcp`, behind
`Authorization: Bearer $VIGIL_MCP_TOKEN`. They only read: an agent can't post
events, change thresholds or silence alerts. The dashboard login doesn't open
`/mcp`, and the MCP token doesn't open the dashboard.

| Tool                 | What it answers                                                     |
|----------------------|---------------------------------------------------------------------|
| `get_system`         | CPU, memory, load and disks right now, with the alert thresholds    |
| `get_system_history` | CPU, memory and load over the last `since` (up to 24 h)             |
| `list_alerts`        | Alerts firing now (failed jobs are one-off notices, not listed)     |
| `list_jobs`          | Per app/kind: totals, last 24 h, failures of the last 7 days, last success and failure, expected interval |
| `list_events`        | Events of the last 24 h (or `since`, up to 7 days), newest first, filtered by app, kind, status |
| `get_run`            | Every event of one run from the last 7 days, with the full error text |

Arguments and results are in [MCP.md](MCP.md). Normally
[mcp-hub](../mcp-hub/SPEC.md) connects to vigil and agents connect to the hub.
To use it from Claude Code directly:

```sh
sudo grep VIGIL_MCP_TOKEN /etc/vigil.env        # on the server
claude mcp add --transport http vigil http://192.168.1.169:8090/mcp \
  --header "Authorization: Bearer <token>"
```

## Limits

- Sample history (CPU/memory charts) lives in memory, so a restart starts the
  24-hour charts fresh. Events are on disk and survive restarts.
- Totals cover the retention period (90 days by default). Older events are
  dropped from the log when vigil starts.
- It watches one machine. For several servers, run one vigil on each.
- Plain HTTP: keep it on the LAN, or put a TLS proxy in front.

## Uninstall

```sh
sudo systemctl disable --now vigil
sudo rm /usr/local/bin/vigil /etc/systemd/system/vigil.service /etc/vigil.env /etc/vigil-client.env
sudo rm -rf /var/lib/private/vigil /var/lib/vigil
sudo systemctl daemon-reload
```
