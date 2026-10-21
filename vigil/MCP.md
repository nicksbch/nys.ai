# vigil MCP tools: spec (approved 2026-10-20; 7-day event window approved 2026-10-21)

vigil serves its data over MCP at `/mcp`, so an agent can ask "is the
server OK?", "did last night's downloads work?" and "why did that fail?".
It is the first upstream of [mcp-hub](../mcp-hub/SPEC.md) and follows the
upstream contract there.

Every tool reads what vigil already keeps in memory: the last 24 hours of
system samples, the last 7 days of events (`-recent`), the per-job totals
and the alerts firing now. No
new storage, no new background work. All six tools only read
(`readOnlyHint: true`). Nothing an agent does through `/mcp` changes vigil.

## Endpoint and auth

- `/mcp` on vigil's existing listener (`[::]:8090` on the server), MCP
  Streamable HTTP, built with `github.com/modelcontextprotocol/go-sdk`.
- `Authorization: Bearer $VIGIL_MCP_TOKEN`. This is a new token, separate
  from `VIGIL_TOKEN`: that one lives in the world-readable
  `/etc/vigil-client.env` because it only lets apps post events. The MCP
  token lets you read everything, so it is kept in the root-only
  `/etc/vigil.env` only.
- New flag `-mcp-token` (default `$VIGIL_MCP_TOKEN`). If it is empty,
  `/mcp` is not served at all (`404`), so a vigil without the token never
  exposes its data without auth by accident.
- The dashboard login (`VIGIL_AUTH`) does not grant `/mcp`, and the MCP
  token does not grant the dashboard.

## Conventions

- **Times** are RFC 3339 in UTC. **Durations** in arguments are Go
  durations (`90m`, `6h`, `24h`).
- **Sizes** are bytes, and disks carry `pct` as well, so the agent doesn't
  have to divide.
- **Results** are JSON objects, returned as structured content with an
  output schema (the SDK's typed `mcp.AddTool`) and as the same JSON in a
  text block for clients that only read text.
- **Bad arguments** (an unknown `status`, `since` over what vigil keeps, a missing
  `run_id`) return `isError: true` with a one-line explanation.
- **Lists** say when they were cut: `"truncated": true` if `limit` stopped
  them.

## Tools

### `get_system`

The server right now. No arguments.

```json
{
  "host": "nas", "now": "2026-10-20T14:03:00Z", "vigil_started": "2026-10-18T09:12:44Z",
  "cpu_pct": 12.4, "mem_pct": 41.0, "load1": 0.62,
  "disks": [{"path": "/", "total": 250000000000, "used": 98000000000,
             "free": 152000000000, "pct": 39.2}],
  "thresholds": {"disk_pct": 85, "cpu_pct": 90, "mem_pct": 90, "sustain": "5m"}
}
```

`cpu_pct`, `mem_pct` and `load1` are `null` where `/proc` is missing
(macOS). A disk that can't be read has `error` set and its numbers at 0.

### `get_system_history`

CPU, memory and load over a window.

| Argument | Default | Rules                                      |
|----------|---------|--------------------------------------------|
| `since`  | `1h`    | Up to `24h`, the history vigil keeps.      |
| `points` | `60`    | 2–240. Samples are reduced to this many, using the same downsampling as the dashboard. |

```json
{"interval": "15s", "samples": [{"t": "2026-10-20T13:03:00Z", "cpu_pct": 8.1,
  "mem_pct": 40.2, "load1": 0.31}, …]}
```

Disk history isn't kept, so it isn't offered.

### `list_alerts`

The alerts firing now. No arguments.

```json
{"alerts": [{"key": "disk:/var/lib/garage", "since": "2026-10-20T11:40:00Z",
             "message": "disk /var/lib/garage is 91% full (45 GB free of 500 GB)"}]}
```

An empty list means nothing is firing. A failed job is not an alert that
stays on: vigil only sends a one-off notice for it, so it doesn't appear
here. Failures are found with `list_jobs` (`failures_24h`) or
`list_events` (`status: failed`).

### `list_jobs`

One row per app and job kind, as the dashboard's job table shows them.

| Argument | Default | Rules                          |
|----------|---------|--------------------------------|
| `app`    | all     | Exact app name.                |

```json
{"jobs": [{"app": "url-to-bucket", "kind": "download",
  "count": 312, "failures": 4, "bytes": 9100000000, "items": 312,
  "last_success": "2026-10-20T02:10:05Z", "last_failure": "2026-10-19T02:11:40Z",
  "last_error": "GET https://…/file.iso: 503 Service Unavailable",
  "count_24h": 6, "failures_24h": 0, "bytes_24h": 4200000000, "items_24h": 6,
  "failures_recent": 1, "expect": "24h"}],
 "recent": "168h"}
```

- Totals cover vigil's whole retention window (90 days by default). The
  `_24h` fields cover the last 24 hours. `failures_recent` covers the
  events vigil keeps in memory, the period given by `recent` (7 days by
  default), so "it failed 6 times this week" is one call.
- `last_error` is the first line only. `get_run` gives the full text.
- A job that is expected (`-expect`) but has never reported is included
  with zero counts, so "it never ran" is visible.

### `list_events`

Events from the last 24 hours by default, or up to 7 days back
(`-recent`), newest first.

| Argument | Default | Rules                                        |
|----------|---------|----------------------------------------------|
| `app`    | all     | Exact app name.                              |
| `kind`   | all     | Exact kind.                                  |
| `status` | all     | `started`, `ok` or `failed`.                 |
| `since`  | `24h`   | Up to `-recent` (`168h` by default).         |
| `limit`  | `50`    | 1–500.                                       |

```json
{"events": [{"time": "2026-10-20T02:10:05Z", "app": "url-to-bucket", "kind": "download",
  "status": "ok", "run_id": "8f3a91c2d4e5f607", "name": "https://…/file.iso -> s3://isos/file.iso",
  "bytes": 1500000000, "items": 1, "duration_ms": 42000}],
 "truncated": false}
```

`error` is cut to its first line here to keep the result small. For the
full text, call `get_run` with the event's `run_id`.

### `get_run`

Every event of one run, oldest first, with the full error text.

| Argument | Rules     |
|----------|-----------|
| `run_id` | Required. |

```json
{"run_id": "8f3a91c2d4e5f607", "events": [{…"status": "started"…}, {…"status": "failed",
  "error": "GET https://…/file.iso: 503 Service Unavailable\n…"}]}
```

Only runs from the last 7 days (`-recent`) can be found, so an older or unknown
`run_id` returns an empty list, not an error. A `started` event with no
result after it means the job may have crashed or hung. The result says so
with `"finished": false` and `"running_for": "3h12m"`.

## Left out on purpose

- **Posting events.** Apps report through relay. An agent shouldn't write
  to the monitoring log.
- **Changing thresholds, `-expect` or silencing alerts.** Those are set at
  deploy time.
- **Events older than `-recent` (7 days).** They are in `events.jsonl`, but
  reading that file on each call would be new code. `list_jobs` covers the
  90-day totals. A longer window is a flag away (`-recent 720h`), at the
  cost of memory; at most 200,000 events are held, the oldest dropped first.

## Code changes

- `mcp.go` (new): the six tools and the `/mcp` handler. It reads the same
  state as `status()` (`m.samples`, `m.disks`, `m.events.snapshot()`,
  `m.alerts.list()`) under the same locks, and adds no fields to
  `monitor`.
- `main.go`: the `-mcp-token` flag, plus one line in `routes()` that mounts
  `/mcp` when the token is set.
- `go.mod`: adds the SDK and raises the `go` line if the SDK needs it.
  `client/` doesn't import the SDK, so apps that use `vigil/client` don't
  get it.
- `README.md`: a short "MCP" section.

About 250–350 lines of Go plus tests.

## Deploy

`install.sh` adds `VIGIL_MCP_TOKEN=<random>` to `/etc/vigil.env` if it
isn't there yet, the same way url-to-bucket adds a missing login. It leaves
the rest of the file alone and prints where to find the token. Then, on the
server, `/etc/mcp-hub.env` gets `VIGIL_MCP_TOKEN=…` and
`/etc/mcp-hub/servers.json` gets:

```json
{"name": "vigil", "url": "http://127.0.0.1:8090/mcp", "token_env": "VIGIL_MCP_TOKEN"}
```

Until the hub exists, Claude Code can use it directly:

```sh
claude mcp add --transport http vigil http://192.168.1.169:8090/mcp \
  --header "Authorization: Bearer $VIGIL_MCP_TOKEN"
```

## Tests

These use an in-process MCP client from the SDK against `routes()`, with a
monitor filled with fixed samples, events and alerts.

- Auth: no token → `401`, wrong token → `401`, no `-mcp-token` set →
  `404`, the dashboard login rejected on `/mcp`.
- Each tool returns the expected JSON for the fixed data.
- `list_events`: each filter, `since`, `limit` and `truncated`, errors cut
  to their first line.
- `get_run`: full error text, unknown `run_id` → empty list, a run with no
  result → `finished: false`.
- Bad arguments → `isError: true` with a message: `since` over the window,
  `points` out of range, an unknown `status`.
- `tools/list`: six tools, all `readOnlyHint: true`, names that fit the
  hub's rules.
