# preprocessor

Unzips archives in the Garage S3 store set up by
`../linux-scripts/garage-setup.sh`. It watches folders (prefixes) in your
buckets and writes every entry of each new `.zip` to another prefix of the same
bucket, so loaders like [duckpipe](../duckpipe) only ever see plain files.

Downloads (from `url-to-bucket`, say) stay exactly as they arrived; the
unzipped files sit next to them.

- **Jobs:** each job maps a bucket + prefix + file pattern to an output
  prefix, e.g. `s3://home/uk/crime/*.zip` → `s3://home/uk/crime/extracted/`.
- **Layout:** each entry is written under its own path in the archive:
  `2026-04.zip`'s `2026-01/2026-01-kent-street.csv` becomes
  `uk/crime/extracted/2026-01/2026-01-kent-street.csv`.
- **Overlapping archives:** an entry already there with the same CRC-32 and
  size is skipped. A monthly download that repeats the last three years only
  writes what changed, and unchanged files keep their ETag, so duckpipe
  doesn't load them again.
- **Once per archive:** an archive is unzipped once per ETag. Replace it with
  new content and it is unzipped again.
- **Lineage:** every file written gets S3 metadata `source` (the archive's
  `s3://` URL) and `crc32`.
- **Bad archives:** a corrupt archive, or one with an entry that would land
  outside the output prefix (`../x`, `/x`), writes nothing. A failure of any
  kind is recorded, so the archive isn't downloaded again on every poll;
  `preprocessor retry` tries it again.
- **Disk:** an archive is downloaded to `/var/lib/preprocessor/tmp` while it
  is unzipped, then deleted. The server needs room for the largest one.
- **Monitoring:** archives and failures are published to [relay](../relay)
  when relay is installed, and [vigil](../vigil) shows them.
- **Page:** a web page (port 8099) shows each job's archives, what is being
  unzipped and the history, and can poll now, extract an archive again and
  remove records of past attempts. See [The page](#the-page).

It looks for new archives every minute (`poll`). `sudo systemctl kill -s HUP
preprocessor` makes it look now.

## Files

| File                | What it is                                                    |
|---------------------|---------------------------------------------------------------|
| `main.go`           | Flags and commands                                            |
| `config.go`         | `preprocessor.yaml`: jobs, checks, which keys match           |
| `unzip.go`          | Polling, unzipping, status, history, retry                    |
| `state.go`          | `state.json`: every archive unzipped or failed                |
| `ui.go`             | The web page's server: JSON API, poll now, redo, remove       |
| `ui.html`           | The page (plain HTML and JavaScript, built into the binary)   |
| `preprocessor.yaml` | The jobs, installed on the server as `/etc/preprocessor.yaml` |
| `deploy.sh`         | Build here, install on the server                             |
| `install.sh`        | Runs on the server: key, user, config, systemd unit           |
| `undeploy.sh`       | Remove it from the server                                     |

## Config

```yaml
poll: 1m             # default 1m

jobs:
  - name: uk_crime                # letters, digits, . - _
    bucket: home
    prefix: uk/crime/             # optional: the whole bucket
    pattern: "*.zip"              # optional, the default
    to: uk/crime/extracted/       # required, same bucket
```

| Field     | Default     | Meaning                                                                              |
|-----------|-------------|--------------------------------------------------------------------------------------|
| `prefix`  | whole bucket| Folder to watch, including subfolders                                                |
| `pattern` | `*.zip`     | Glob on the file name. With a `/`, on the path below the prefix (`*/crime-*.zip`)    |
| `to`      |             | Prefix the entries are written to. Keys below it are never read as archives          |

`to` may be inside `prefix` (as above), but not the other way round.

## Run it locally

```sh
go run . -config preprocessor.yaml check    # the config loads?
go run . -config preprocessor.yaml -state-dir /tmp/pp once
go run . -state-dir /tmp/pp status
go run . -config preprocessor.yaml -state-dir /tmp/pp run   # page on http://127.0.0.1:8099
```

This writes to the real bucket. Keep `-state-dir` separate from the server's:
the server has its own `state.json` and would unzip the same archives again
(writing nothing new, as every entry is unchanged).

## Deploy to the server

```sh
ONLY=preprocessor ../deploy-all.sh   # or ./deploy.sh
```

`deploy.sh` checks `preprocessor.yaml`, builds for the server's
architecture, copies the binary, `install.sh` and `preprocessor.yaml` over
ssh, and runs `install.sh` with sudo. Settings (in `../deploy.env` with the
`PREPROCESSOR_` prefix, or as env vars to `deploy.sh`):

| Setting    | Default                 | Meaning                                         |
|------------|-------------------------|-------------------------------------------------|
| `BUCKETS`  | `home`                  | Buckets the key may read and write              |
| `KEY_NAME` | `preprocessor-key`      | Garage key the service uses                     |
| `ENDPOINT` | `http://127.0.0.1:3900` | S3 endpoint, as seen from the server (first install only) |
| `LISTEN`   | `[::]:8099`             | The web page; `127.0.0.1:8099` for local only   |
| `UI_USER`  | `admin`                 | Basic-auth user for the page (first install only) |

### What `install.sh` does (on the server)

1. Stops early if Garage isn't installed and running.
2. Checks the new `preprocessor.yaml` with the new binary; if it doesn't
   load, nothing is replaced. Installs `/usr/local/bin/preprocessor` and the
   `preprocessorctl` helper.
3. Creates a `preprocessor` system user, which owns `/var/lib/preprocessor`.
4. Creates a key, `preprocessor-key`, that can read and write `BUCKETS`. On
   the first run only, it writes `/etc/preprocessor.env` (the key, root only).
   It adds a login for the page (`PREPROCESSOR_UI_AUTH`) if there isn't one,
   and prints it once.
5. Installs `preprocessor.yaml` from the repo as `/etc/preprocessor.yaml`,
   every time.
6. Writes `/etc/systemd/system/preprocessor.service`, then enables and
   restarts it, and waits until it is watching and the page answers.

## After deploying

- **Change your jobs:** edit `preprocessor.yaml` here and deploy. Don't edit
  `/etc/preprocessor.yaml` on the server: the next deploy replaces it. A job's
  bucket must be one of `BUCKETS`.
- **Page:** `http://<server>:8099`; login: `sudo grep PREPROCESSOR_UI_AUTH /etc/preprocessor.env`
- **Status:** `sudo preprocessorctl status`
- **Retry failed archives:** `sudo preprocessorctl retry [job]`
- **History:** `sudo preprocessorctl history [job]`
- **Poll now:** `sudo systemctl kill -s HUP preprocessor`
- **Logs:** `journalctl -u preprocessor -f`

Use `preprocessorctl` rather than running `preprocessor` as root: it runs as
the service's user, so it can't leave a root-owned `state.json` behind that
the service can't write.

## The page

`preprocessor run` serves it on `-listen` (default `127.0.0.1:8099`, empty for
none), with basic auth from `-auth` / `PREPROCESSOR_UI_AUTH`.

- **Jobs:** each job from `/etc/preprocessor.yaml`, read-only (change jobs in
  the repo and deploy), with the archives its last poll found: waiting for
  the next poll, unzipping, unzipped or failed, and how many files the last
  run wrote.
- **Poll now:** like `systemctl kill -s HUP`; a poll already running finishes
  first.
- **Extract again:** marks the archive's records `redone` and polls, so it is
  unzipped again even though its ETag hasn't changed (say, after the extracted
  files were deleted). Entries already there and unchanged are still skipped.
  Redone records stay in the history.
- **History:** every record in `state.json`, newest first. **Remove** deletes
  one, e.g. failures of archives that are no longer in the bucket. The record
  that keeps an archive still in the bucket from being unzipped again can't
  be removed (that would be "Extract again"), and until the first poll none
  can, because it isn't known yet which archives are in the bucket. Removing
  a record never deletes files from the bucket.

## relay and vigil

When `RELAY_URL` is set or `/etc/relay-client.env` exists, preprocessor
publishes to relay, and vigil shows the events as app `preprocessor`:

| Topic                | Types                       | When                                                  |
|----------------------|-----------------------------|-------------------------------------------------------|
| `preprocessor.unzip` | `started`, `ok` / `failed`  | Each archive; same run ID. `items` = files written    |
| `preprocessor.poll`  | `failed`, then `ok`         | A job (or `state.json`) starts / stops failing to poll |

`data` holds `name` (`job: archive -> output prefix`), `job`, `bucket`,
`key`, `to`, `bytes` (the archive), `entries`, `written`, `skipped`,
`duration_ms` and, on failure, `error`.

## Limits

- Zip only. Other formats (`.tar.gz`, …) aren't unzipped.
- Archives inside archives are written as files, not unzipped.
- Entries are written to the archive's bucket only.
- One archive at a time, per poll.

## Uninstall

```sh
./undeploy.sh          # keeps the config, state and everything it unzipped
PURGE=1 ./undeploy.sh  # also the config, state and user; buckets are never touched
```
