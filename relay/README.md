# relay

A small event log with subscriptions. Applications **publish** events, such
as "url-to-bucket download started" and then "… ok". Other services and
agents **read** them: live, by waiting for one particular event, or from
where they last stopped through a named consumer whose cursor the server
keeps. It is one Go binary with no dependencies, and it is both the server
and the CLI.

It is built so an autonomous agent can use it with nothing but `curl` or the
`relay` CLI, and with no state of its own:

- `GET /` describes the whole API.
- `relay wait` blocks until a result arrives and exits with a meaningful code.
- `relay listen` runs a command for each event.
- The output is JSON lines.

The design and its reasons are in [SPEC.md](SPEC.md).

## Files

| File               | What it is                                                          |
|--------------------|---------------------------------------------------------------------|
| `main.go`          | Subcommand dispatch and `relay serve`                               |
| `store.go`         | The event log: daily JSON-lines segments, in-memory index, retention |
| `consumers.go`     | Named consumers (filter + cursor), kept in `consumers.json`         |
| `server.go`        | HTTP API, long-polling                                              |
| `cli.go`           | The CLI commands                                                    |
| `usage.txt`        | Usage text, served at `GET /` and printed by `relay help`           |
| `ui.html`          | Read-only dashboard, served at `GET /ui`                            |
| `client/client.go` | Go package your apps import to publish events                       |
| `deploy.sh`        | Run on your Mac: builds the binary and installs it on the server    |
| `install.sh`       | Run on the server by `deploy.sh`: binary, token, systemd service    |

## Run it locally

```sh
go build -o relay . && ./relay serve -data ./data -token dev
export RELAY_URL=http://127.0.0.1:8091 RELAY_TOKEN=dev
./relay pub demo.job started -run-id r1
./relay tail
```

| Flag         | Default                         | Meaning                                    |
|--------------|---------------------------------|--------------------------------------------|
| `-listen`    | `127.0.0.1:8091`                | Address to serve on                        |
| `-data`      | `$STATE_DIRECTORY` or `./data`  | Where `events/` and `consumers.json` live  |
| `-retention` | `720h` (30 days)                | How long events are kept                   |
| `-token`     | `$RELAY_TOKEN`                  | Bearer token required on `/v1`             |

## Deploy to the server

```sh
./deploy.sh
# with settings:
LISTEN=127.0.0.1:8091 EXTRA_ARGS="-retention 2160h" ./deploy.sh
```

| Variable     | Default         | Meaning                                                   |
|--------------|-----------------|-----------------------------------------------------------|
| `HOST`       | `192.168.1.169` | SSH destination                                           |
| `SSH_PORT`   | `2222`          | SSH port on the server                                    |
| `LISTEN`     | `[::]:8091`     | Where relay listens. `127.0.0.1:8091` keeps it local      |
| `EXTRA_ARGS` |                 | Any other flags, e.g. `-retention 2160h`                  |

The first run writes two files. Later runs leave them alone:

- `/etc/relay.env` (root only): the token, for the service.
- `/etc/relay-client.env` (readable by everyone): `RELAY_URL` and
  `RELAY_TOKEN`. The Go client and the `relay` CLI read it by themselves, so
  apps, cron jobs and agent sessions on the server need no setup.

The binary is installed as `/usr/local/bin/relay`, so the CLI works from any
shell. The service runs as a throwaway user (`DynamicUser=yes`) with the same
hardening as vigil, and keeps its data in `/var/lib/relay`.

After deploying:

- **Usage:** `relay help`, or `curl http://192.168.1.169:8091/`
- **Logs:** `journalctl -u relay -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8091/tcp`

---

# Using it

## Publish

From a shell:

```sh
relay pub url-to-bucket.download started -run-id abc -data '{"url":"https://…"}'
relay pub url-to-bucket.download ok -run-id abc -cause 1 -data @result.json
some-command | relay pub my-app.report ok -data -
```

With `curl`:

```sh
curl -fsS -X POST "$RELAY_URL/v1/events" -H "Authorization: Bearer $RELAY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"topic":"my-app.ingest","type":"ok","run_id":"abc","data":{"rows":120}}'
```

From Go, with the `relay/client` package. A nil client (when nothing is
configured) turns every call into a no-op, so the app builds and runs the
same on your Mac:

```sh
go mod edit -require=relay@v0.0.0 -replace=relay=../relay
```

```go
import relay "relay/client"

bus := relay.FromEnv() // RELAY_URL, RELAY_TOKEN or /etc/relay-client.env; nil if unset
_, err := bus.Publish(ctx, relay.Event{Topic: "my-app.ingest", Type: "ok", RunID: id,
	Data: map[string]any{"rows": 120}})   // waits up to 3 s; log err, never fail on it
bus.Go(relay.Event{…})                  // in a server: in the background
```

Conventions: use `<app>.<kind>` as the topic and `started`, `ok` or `failed`
as the type, with the failure text in `data.error`. Give `run_id` the same
value on every event of a run. Set `key` when a retry could publish the same
event twice.

## Read

```sh
relay tail -topic 'url-to-bucket.*' -n 50     # recent events
relay tail -f                                  # and follow
relay topics                                   # what exists
relay wait -run-id abc -type ok,failed -timeout 30m   # 0 ok, 1 failed, 3 timeout
```

## Subscribe

Each service that wants events gets its own consumer. The server remembers
how far it got:

```sh
relay consumer indexer -topic 'url-to-bucket.*' -type ok   # once; starts at the newest event
relay listen indexer -- ./index-new-file.sh                # event JSON on stdin
```

`listen` acks an event when the command exits 0. Otherwise it retries the
same event after 5 s, doubling up to 5 min, so nothing is skipped silently.
To consume by hand, or from an agent:

```sh
relay next indexer -wait 30s    # prints the next event; exit 3 if none
relay ack indexer 1042          # done with everything up to 1042
```

Delivery is at least once, so handlers should be safe to run twice on the
same event.

## Publishers

- **url-to-bucket** publishes `url-to-bucket.download`: first `started`,
  with `data: {name, url, bucket, key}`, then `ok` or `failed`, which add
  `bytes`, `duration_ms` and (for failures) `error`. Both share a `run_id`.
  As a service it also reads that topic: publish a `requested` event with
  `data: {url, bucket, key?}` and it runs the download, with your `run_id`
  and `cause` set to the request (see `url-to-bucket/serve.go`).
- **preprocessor** publishes `preprocessor.unzip` and `preprocessor.poll`.
- **duckpipe** publishes `duckpipe.ingest` and `duckpipe.poll`.
- **lakeshape** publishes `lakeshape.run`, `lakeshape.model` and
  `lakeshape.project`, and reads its triggers through consumer `lakeshape`.
- **foreman** publishes `foreman.<task type>` for tasks, runs and actions.
- **mcp-hub** publishes `mcp-hub.call` for every tool call.

Each one's README has its events and their `data`.

## Dashboard

`http://192.168.1.169:8091/ui` shows the topics, the consumers with their
backlogs, and the events as they arrive. Filter by topic, type or run ID, or
click a topic, type or run to filter by it. Click an event to see all of it.
It is read-only: publishing, acking and deleting stay in the CLI and API.

The page asks for the relay token once (it is in `/etc/relay-client.env` on
the server) and keeps it in that browser's local storage. "Forget token"
removes it.

## vigil reads from relay

vigil keeps a consumer named `vigil` (topic `*`, types `started,ok,failed`)
and turns each event into one of its own: topic `<app>.<kind>` gives vigil's
app and kind, the type its status, and `data.name`, `bytes`, `items`,
`duration_ms` and `error` its fields. So any app that follows the
conventions above shows up on vigil's dashboard, with alerts on failures,
without talking to vigil. Deploy relay before vigil: vigil reads
`/etc/relay-client.env` when it starts.

## API

`GET /` (or `relay help`) has the full reference. In short:

| Method & path                     | What it does                                                |
|-----------------------------------|-------------------------------------------------------------|
| `POST /v1/events`                 | Publish. `201`, or `200` with the original if `key` repeated |
| `GET /v1/events`                  | `topic`, `type`, `run_id`, `after`, `limit`, `wait` (≤ 60 s), or `last=N` |
| `GET /v1/topics`                  | Topics, their types, counts and last event                  |
| `PUT /v1/consumers/{name}`        | `{"topic","type","from"}`                                   |
| `GET /v1/consumers`               | Cursors and backlogs                                        |
| `GET /v1/consumers/{name}/next`   | `limit`, `wait`. The cursor does not move                   |
| `POST /v1/consumers/{name}/ack`   | `{"id":…}`                                                  |
| `DELETE /v1/consumers/{name}`     |                                                             |
| `GET /ui`                         | Read-only dashboard (see below)                             |
| `GET /healthz`                    | No auth                                                     |

## Limits

- Built for a home server: up to tens of thousands of events a day. The
  index of every kept event is held in memory, at about 100 bytes per event.
- Each consumer name is for one reader at a time. There are no leases for
  sharing work between several workers.
- Plain HTTP with one shared token: keep it on the LAN, like vigil.

## Uninstall

```sh
sudo systemctl disable --now relay
sudo rm /usr/local/bin/relay /etc/systemd/system/relay.service /etc/relay.env /etc/relay-client.env
sudo rm -rf /var/lib/private/relay /var/lib/relay
sudo systemctl daemon-reload
```
