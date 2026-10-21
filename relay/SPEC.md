# relay: spec (approved 2026-10-19)

A small event log with subscriptions. Applications **publish** events
("url-to-bucket download started", "… ok"). Other services and agents
**read** them, either live or from where they last stopped. It is one Go
binary with no dependencies outside the standard library. The same binary is
both the server and the CLI.

## Why not vigil, NATS, Redis…

- **vigil** answers "is everything healthy?". It aggregates events, raises
  alerts and has a dashboard. It does not deliver events to anyone. Adding
  subscriptions would make it do two jobs.
- **NATS, Redis Streams, Kafka** do far more than needed. They have no
  notion of "an agent wakes up, asks what happened since last time, does the
  work, and acknowledges it" without a client library and some setup.

## What "agent-native" means here

The goal is that an autonomous agent (Claude Code, a scheduled routine, a
shell script) can use relay with nothing but `curl` or the `relay` CLI, and
with no state of its own:

1. **Self-describing.** `GET /` returns plain-text usage. `GET /v1/topics`
   lists every topic with its event types and the time of its last event, so
   an agent can find out what exists.
2. **The server keeps the cursor.** A named consumer (`relay next indexer`)
   picks up where it left off, even if the agent process is new each time.
3. **Wait instead of poll.** `relay wait --run-id X --type ok,failed --timeout 30m`
   blocks until the event arrives and then exits with a meaningful code. That
   is one tool call for "kick off the download and tell me how it went".
4. **Retries are safe.** An optional idempotency `key` makes a retried
   publish return the original event instead of creating a duplicate.
5. **Causality.** `run_id` ties together the events of one run. `cause`
   points at the event that triggered this one. An agent can follow a chain:
   download ok → ingest started → ingest ok.
6. **Machine-first output.** The CLI prints JSON lines and uses fixed exit
   codes. Errors are JSON as well.
7. **Triggering.** `relay listen <consumer> -- <cmd>` runs a command for each
   event, passing the event on stdin, and acknowledges it when the command
   exits 0. That is how an event wakes up an agent or a script.

## Event

```json
{
  "id": 1042,
  "time": "2026-10-19T14:03:11.402Z",
  "topic": "url-to-bucket.download",
  "type": "ok",
  "run_id": "8f3a91c2d4e5f607",
  "cause": 1040,
  "key": "dl-8f3a91c2d4e5f607-ok",
  "data": {"url": "https://…/file.iso", "bucket": "isos", "key": "file.iso",
           "bytes": 1500000000, "duration_ms": 42000}
}
```

| Field    | Set by    | Rules                                                                     |
|----------|-----------|---------------------------------------------------------------------------|
| `id`     | server    | Increases with each event and is never reused. It is the cursor.          |
| `time`   | server    | When the event was stored.                                                |
| `topic`  | publisher | Required. Lowercase dot-separated segments of `a-z 0-9 _ -`, up to 128 characters. Convention: `<app>.<kind>`. |
| `type`   | publisher | Required. 1–64 characters of `A-Z a-z 0-9 _ . -`. Convention for jobs: `started`, `ok`, `failed`. Anything else is allowed (`requested`, `skipped`…). |
| `run_id` | publisher | Optional. Same character rules as `type`. Same value for every event of one run. |
| `cause`  | publisher | Optional. The `id` of the event that led to this one.                    |
| `key`    | publisher | Optional idempotency key, up to 128 characters. A repeat within 24 h returns the original event. |
| `data`   | publisher | Optional JSON object, up to 64 KB. relay never reads it.                 |

For a failed job, the convention is `data.error` (text), as in vigil.

## Topic filters

These are used wherever a filter is accepted:

- `url-to-bucket.download`: that topic only
- `url-to-bucket.*`: every topic that starts with `url-to-bucket.`
- `*`: everything
- a comma-separated list of the above

## HTTP API

All endpoints except `/` and `/healthz` require `Authorization: Bearer $RELAY_TOKEN`.

| Method & path                         | What it does                                                                |
|---------------------------------------|-----------------------------------------------------------------------------|
| `GET /`                               | Plain-text usage (this API, the CLI, examples)                              |
| `GET /healthz`                        | `ok`                                                                        |
| `POST /v1/events`                     | Publish. Returns `201` with the stored event, or `200` with the original if `key` repeats. The event is fsynced before the response. |
| `GET /v1/events`                      | Read. Query: `topic`, `type`, `run_id`, `after` (id, default: latest), `limit` (default 100, max 1000), `wait` (long-poll, up to `60s`), or `last=N` for the newest N (used by `relay tail`). Returns `{"events":[…],"next":<id>}`. |
| `GET /v1/topics`                      | `[{"topic","types":[…],"count","last_id","last_time"}]`                     |
| `PUT /v1/consumers/{name}`            | Create or update a consumer: `{"topic":"…","type":"ok,failed","from":"latest"\|"earliest"\|<id>}` |
| `GET /v1/consumers`                   | List consumers with their filter, cursor and backlog (events not yet acked) |
| `GET /v1/consumers/{name}/next`       | The next events after the consumer's cursor that match its filter. Query: `limit` (default 1), `wait`. The cursor does **not** move. |
| `POST /v1/consumers/{name}/ack`       | `{"id":1042}`: moves the cursor to that id                                  |
| `DELETE /v1/consumers/{name}`         | Removes the consumer                                                        |

Delivery is **at least once**. An event is delivered again until it is
acked. Each consumer name is meant for one reader at a time. Several services
that want the same events each use their own consumer name (fan-out).

If a consumer's cursor falls behind the retention window, `next` resumes
from the oldest event still kept and adds `"gap":true` to the response.

Errors are `{"error":"…"}` with a 4xx or 5xx status.

## CLI

The binary reads `RELAY_URL` and `RELAY_TOKEN`, or `/etc/relay-client.env`,
the same way as vigil's client. Output is JSON lines.

```sh
relay serve [-listen 127.0.0.1:8091] [-data ./data] [-retention 720h]

relay pub <topic> <type> [-run-id ID] [-cause ID] [-key K] [-data JSON|@file|-]
relay tail [-topic F] [-type T] [-run-id ID] [-after ID] [-n 20] [-f]   # -f: follow
relay wait [-topic F] [-type T] [-run-id ID] [-after ID] [-timeout 10m]
relay topics

relay consumer <name> [-topic F] [-type T] [-from latest|earliest|ID]   # create/update
relay consumers
relay next <name> [-n 1] [-wait 30s]
relay ack <name> <id>
relay listen <name> [-wait 60s] -- <cmd> [args…]
```

Exit codes:

| Code | Meaning                                                                 |
|------|-------------------------------------------------------------------------|
| 0    | Success. For `wait`: a matching event arrived and was printed.          |
| 1    | `wait` matched an event whose type is `failed`. It is still printed.    |
| 2    | Usage error, or the server could not be reached or rejected the request. |
| 3    | `wait` timed out, or `next` found nothing within `-wait`.               |

`wait` with no `-after` starts from the latest event. For a run started just
before, give `-after` or `-run-id` so an event that already arrived is not
missed. With `-run-id`, `wait` also looks back through the existing events
of that run.

`listen` runs `<cmd>` once per event with the event JSON on stdin, plus
`RELAY_EVENT_ID`, `RELAY_TOPIC`, `RELAY_TYPE` and `RELAY_RUN_ID` in the
environment. If the command exits 0, the event is acked. Otherwise listen
logs the failure, waits (5 s, doubling up to 5 min) and runs the command
again on the same event, so one bad event blocks that consumer. That is
deliberate: nothing is skipped silently. Ctrl-C or SIGTERM stops it cleanly.

## Go client: `relay/client`

This mirrors vigil's client. A nil client turns every call into a no-op, so
an application never fails because relay is down or not configured. All
calls use a 3 s timeout.

```go
import relay "relay/client"

bus := relay.FromEnv()   // RELAY_URL, RELAY_TOKEN or /etc/relay-client.env; nil if unset
ev, err := bus.Publish(ctx, relay.Event{Topic: "url-to-bucket.download", Type: "started",
	RunID: id, Data: map[string]any{"url": u}})
bus.Go(relay.Event{…})   // in the background, ignoring errors (servers)
```

Reading is left to the HTTP API and the CLI in v1. A Go subscriber helper
would be added once a Go service actually needs it.

## Storage

- Daily segment files, `data/events/2026-10-19.jsonl`, with one event per
  line. Each append is fsynced.
- On startup, segments are scanned to rebuild an in-memory index (id →
  segment and offset), the topic table and the idempotency keys from the
  last 24 h. Reads seek straight into the file.
- Retention (`-retention`, default 30 days) deletes whole segments once
  they are older than that. It runs at startup and then hourly.
- Consumers live in `data/consumers.json`, rewritten atomically
  (temp file + rename) on every ack.
- Scale target: a home server, up to tens of thousands of events a day
  (the index costs about 100 bytes per kept event). One process
  with one writer lock and no clustering.

## Deploy

`deploy.sh` and `install.sh` follow the same pattern as vigil:

- A systemd service with `DynamicUser=yes` and the same hardening, state in
  `/var/lib/relay`, listening on `[::]:8091`.
- The first run writes `/etc/relay.env` (root only, containing the token)
  and `/etc/relay-client.env` (readable by everyone: `RELAY_URL`,
  `RELAY_TOKEN`). Later runs leave both alone.
- The `relay` binary also goes to `/usr/local/bin`, so the CLI works from
  any shell, cron job or agent session on the server.

## First integration: url-to-bucket

url-to-bucket publishes to `url-to-bucket.download`:

- `started` with `data: {url, bucket, key}`
- then `ok` with `{url, bucket, key, bytes, duration_ms}`, or `failed` with
  `{url, bucket, key, bytes, duration_ms, error}`

Both events share a random `run_id`, and `data.name` is the
`url -> s3://bucket/key` line vigil shows. Publishing uses the blocking
`Publish(ctx, …)` with a 3 s timeout, so the events aren't lost when the CLI
exits. A failure only prints a warning.

## Dashboard (added 2026-10-19)

`GET /ui` serves one embedded HTML page (`ui.html`) with no auth. It holds no
data: it asks for the relay token, keeps it in the browser's local storage,
and reads `/v1/topics`, `/v1/consumers` and `/v1/events` (`last=200`, then
long-polling with `wait=30s`). It shows topics, consumers with their
backlogs, and a live event list that can be filtered by topic, type and run
ID. It is read-only and adds no server endpoints. `GET /` stays plain text
for agents.

## Out of scope for v1

- Several workers sharing one consumer with leases or visibility timeouts
  (a work queue). v1 is a log with fan-out subscriptions.
- Server-side push (webhooks, SSE, WebSockets). `listen` and long-poll
  cover this.
- Schemas or validation of `data`, exactly-once delivery, clustering,
  replication and TLS (LAN only, like vigil).
- A dashboard that changes anything. A read-only one was added on
  2026-10-19 (see below).

## Size

About 700–900 lines of Go plus tests, standard library only:
`main.go` (CLI and flags), `server.go` (HTTP), `store.go` (segments, index,
retention), `consumers.go`, `client/client.go`, plus `README.md`,
`deploy.sh` and `install.sh`.

## Tests

- Store: append and replay, index rebuilt after a restart, retention
  dropping segments, idempotency key dedupe, recovery from a truncated last
  line after a crash.
- API: publish/read with filters, long-poll wakes on publish and times out,
  consumer next/ack/redelivery, the gap flag.
- CLI: `wait` exit codes 0, 1 and 3; `listen` acks on 0 and retries on
  non-zero.

## Open questions

1. **Name.** `relay` is a placeholder. Alternatives: `bus`, `herald`, `wire`.
2. **vigil.** Done: vigil is a relay consumer (`vigil/relay.go`), and
   url-to-bucket and duckpipe publish only to relay.
3. **Retention.** 30 days by default (vigil keeps 90). Is that enough?
4. **Port.** 8091, next to vigil's 8090.
