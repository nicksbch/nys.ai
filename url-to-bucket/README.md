# url-to-bucket

Downloads a URL straight into a bucket of the Garage S3 store set up by
`../linux-scripts/garage-setup.sh`, without writing it to disk. It is where
the data pipeline starts: [preprocessor](../preprocessor) and
[duckpipe](../duckpipe) pick up what it writes. The same Go binary is a CLI
and a service.

- **Streams:** the file goes from the HTTP response to S3 in 16 MB parts,
  so memory stays at a few parts whatever the file's size, and no disk is
  needed.
- **Requests through relay:** the service runs downloads asked for as
  `requested` events on [relay](../relay), up to 2 at a time. Its web page,
  its MCP tool and `relay pub` all ask this way.
- **Results on relay:** each download publishes `started`, then `ok` or
  `failed`, so [vigil](../vigil) shows it, foreman's on-call agent picks up
  failures, and anyone can `relay wait` for it.
- **Page:** a form to start a download, the downloads in progress with live
  bytes, speed and ETA, and the recent ones with their errors (see
  [The page](#the-page)).
- **MCP:** `download`, plus three read-only tools that say where a file
  should go, for agents through [mcp-hub](../mcp-hub) (see [MCP](#mcp)).
- **Survives restarts:** a request whose download was cut short by a
  restart runs again, with the same run ID.

## Files

| File          | What it is                                                                  |
|---------------|-----------------------------------------------------------------------------|
| `main.go`     | The CLI, and one download: HTTP to S3, progress, relay events               |
| `serve.go`    | `serve`: reading requests from relay, the workers, acking                   |
| `ui.go`       | The page's server: JSON API, the downloads list from relay, the login       |
| `ui.html`     | The page (plain HTML and JavaScript, built into the binary)                 |
| `mcp.go`      | The MCP tools at `/mcp`                                                     |
| `suggest.go`  | `suggest_key`: matching a file name against what is in the bucket           |
| `SPEC-mcp.md` | The MCP tools' spec                                                         |
| `deploy.sh`   | Run on your Mac: builds the binary and installs it on the server            |
| `install.sh`  | Run on the server by `deploy.sh`: binary, key, login, MCP token, service    |

## Keys

The object key is where the file goes in the bucket:

| `key`                    | The file goes to                       |
|--------------------------|----------------------------------------|
| empty                    | the URL's file name, at the top        |
| ends in `/` (`isos/`)    | that folder, with the URL's file name  |
| anything else            | exactly that key                       |

`https://…/debian-13.iso` with key `isos/` is `isos/debian-13.iso`. An
existing object at the key is replaced.

## Run it locally

```sh
go build -o url-to-bucket .
./url-to-bucket https://example.org/file.iso home isos/   # progress, speed and ETA on stderr
```

Credentials come from the usual AWS environment variables or your `~/.aws`
profile, as for the other tools:

```sh
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
AWS_DEFAULT_REGION=garage
AWS_ENDPOINT_URL=http://192.168.1.169:3900
```

| Flag            | Default             | Meaning                                       |
|-----------------|---------------------|-----------------------------------------------|
| `-endpoint-url` | `$AWS_ENDPOINT_URL` | S3 endpoint                                   |
| `-profile`      |                     | Profile name from `~/.aws`                    |
| `-content-type` | the server's        | Content-Type to store instead of the server's |

Flags may come anywhere on the line. With `RELAY_URL` and `RELAY_TOKEN` set
(or `/etc/relay-client.env` present), a CLI run publishes its events like
the service does. Ctrl-C stops it and exits 130.

The service needs relay:

```sh
RELAY_URL=http://127.0.0.1:8091 RELAY_TOKEN=dev ./url-to-bucket serve   # page on http://127.0.0.1:8093
```

| Flag (`serve`)  | Default                    | Meaning                                         |
|-----------------|----------------------------|-------------------------------------------------|
| `-workers`      | `2`                        | Downloads at the same time                      |
| `-listen`       | `127.0.0.1:8093`           | Address of the page and `/mcp`; empty for none  |
| `-auth`         | `$URL_TO_BUCKET_UI_AUTH`   | Basic auth for the page, as `user:pass`         |
| `-mcp-token`    | `$URL_TO_BUCKET_MCP_TOKEN` | Bearer token for `/mcp`. Empty: no `/mcp`       |
| `-consumer`     | `url-to-bucket`            | relay consumer name                             |
| `-endpoint-url`, `-profile` |                | As for the CLI                                  |

## Deploy to the server

```sh
./deploy.sh                          # or: ONLY=url-to-bucket ../deploy-all.sh
BUCKETS="home isos" ./deploy.sh      # a new bucket: pass the full list
```

Deploy relay first: the install stops if `/etc/relay-client.env` is
missing.

| Variable   | Default                 | Meaning                                                  |
|------------|-------------------------|----------------------------------------------------------|
| `HOST`     | `192.168.1.169`         | SSH destination                                          |
| `SSH_PORT` | `2222`                  | SSH port on the server                                   |
| `BUCKETS`  | `home`                  | Buckets the key may read and write, granted on every run |
| `KEY_NAME` | `url-to-bucket-key`     | Garage key the service uses                              |
| `WORKERS`  | `2`                     | Downloads at the same time                               |
| `ENDPOINT` | `http://127.0.0.1:3900` | S3 endpoint, as seen from the server (first install only) |
| `LISTEN`   | `[::]:8093`             | The page and `/mcp`; `127.0.0.1:8093` for local only     |
| `UI_USER`  | `admin`                 | Basic-auth user for the page (first install only)        |

`WORKERS` and `LISTEN` are written into the systemd unit on every deploy,
so keep non-default values in `../deploy.env` (`URL_TO_BUCKET_WORKERS`, …).

### What `install.sh` does (on the server)

1. Stops early if Garage isn't running or relay isn't installed.
2. Installs `/usr/local/bin/url-to-bucket`.
3. Creates the key, `url-to-bucket-key`, and lets it read and write each
   bucket in `BUCKETS`.
4. On the first run only, writes `/etc/url-to-bucket.env` (root only) with
   that key. It adds a login for the page (`URL_TO_BUCKET_UI_AUTH`, printed
   once) and an MCP token (`URL_TO_BUCKET_MCP_TOKEN`) if they are missing.
   mcp-hub's install copies the token.
5. Writes `/etc/systemd/system/url-to-bucket.service` (`DynamicUser`, the
   same hardening as the other services), then enables and restarts it.
6. Waits until the service reads from relay and the page answers.

## After deploying

- **Page:** `http://192.168.1.169:8093`; login: `sudo grep URL_TO_BUCKET_UI_AUTH /etc/url-to-bucket.env`
- **Download from a shell:**
  `relay pub url-to-bucket.download requested -run-id ID -data '{"url":"…","bucket":"home","key":"isos/"}'`,
  then `relay wait -run-id ID -type ok,failed` (exit 0 ok, 1 failed)
- **From a chat:** [nexus](../nexus)
- **Logs:** `journalctl -u url-to-bucket -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8093/tcp`
- **Home page:** has a `url-to-bucket` card

## The page

- **New download:** URL, bucket (the buckets the key can see are offered)
  and an optional key. **Download** publishes a `requested` event, as
  `relay pub` would.
- **In progress:** each download running now, with bytes, total, speed and
  ETA, refreshed every second.
- **Recent:** the last 100 downloads from relay's last 500 events on the
  topic: queued, running, done (size, time) or failed (the error).

Every change needs an `X-Url-To-Bucket` header that only the page sends,
so other websites can't start downloads through your browser.

## Requests and events

All on topic `url-to-bucket.download`, sharing the run's `run_id`:

| Type        | Who publishes it          | `data`                                                     |
|-------------|---------------------------|------------------------------------------------------------|
| `requested` | the page, MCP, `relay pub` | `url` and `bucket` (required), `key`, `content_type`      |
| `started`   | the service or CLI        | `name` (`url -> s3://bucket/key`), `url`, `bucket`, `key`  |
| `ok`        | the service or CLI        | the above, `bytes`, `duration_ms`                          |
| `failed`    | the service or CLI        | the above, `bytes`, `duration_ms`, `error`                 |

- A request without a `run_id` gets `req-<request id>`. The result's
  `cause` is the request's ID.
- A request that isn't valid (no bucket, not an http or https URL) gets a
  `failed` only.
- The service's relay consumer, `url-to-bucket`, is acked up to the newest
  request with all earlier ones done. A download cut short by a restart
  publishes no result and runs again after it, and an event repeated that
  way is stored once.
- A response other than 2xx fails the download, with its status in `error`.

## MCP

At `/mcp` on the page's address, behind `Authorization: Bearer
$URL_TO_BUCKET_MCP_TOKEN` (the page's login doesn't open it). Agents reach
them through mcp-hub as `url-to-bucket__*`; the `oncall` profile has
`download`, and the `nexus` profile has all four.

| Tool           | Read-only | What it does                                                          |
|----------------|-----------|-----------------------------------------------------------------------|
| `download`     | no        | `{url, bucket, key?, content_type?}`: publishes a `requested` event and returns `{run_id, topic}` at once. `_meta.wait` is `2h` |
| `list_buckets` | yes       | The buckets the key can see                                           |
| `list_objects` | yes       | `{bucket, prefix?}`: one level, like `ls`; at most 200 entries, with `truncated` |
| `suggest_key`  | yes       | `{bucket, url}`: the folders already holding files named like the URL's file (`2023-08.zip` matches `####-##.zip`), best first, each with the full key to use. Reads at most 20,000 keys |

Details in [SPEC-mcp.md](SPEC-mcp.md).

## Limits

- One GET, no resuming: a download that breaks off fails and starts over
  when asked again.
- No schedule: downloads run when something asks for one.
- Only `http` and `https` URLs, with no login or custom headers.
- The page's list covers relay's last 500 events on the topic, so within
  relay's retention (30 days by default).
- Plain HTTP, like the other services: keep it on the LAN.

## Uninstall

```sh
sudo systemctl disable --now url-to-bucket
sudo rm /usr/local/bin/url-to-bucket /etc/systemd/system/url-to-bucket.service /etc/url-to-bucket.env
sudo systemctl daemon-reload
sudo garage key delete --yes url-to-bucket-key
```

Its relay consumer stays in relay; to remove it:
`curl -X DELETE -H "Authorization: Bearer $RELAY_TOKEN" $RELAY_URL/v1/consumers/url-to-bucket`.
