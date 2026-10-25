# duckpipe

Snowpipe for the Garage S3 store set up by `../linux-scripts/garage-setup.sh`.
It watches folders (prefixes) in your buckets and loads every new file into a
DuckDB table, a few seconds after it lands (for example from `url-to-bucket`).

The tables stay on S3. They are a [DuckLake](https://ducklake.select): the data
is Parquet files in a bucket (`s3://lake/` by default), and a small catalog
(a PostgreSQL database on the server, set up by
`../linux-scripts/ducklake-setup.sh`) lists the tables, their columns and
every snapshot. Any DuckDB 1.5+ can query them, with time travel.

duckpipe itself is plain Go. It does the loading by running the official
`duckdb` CLI (1.5 or newer), which `install.sh` puts on the server and which
you can also use there to query the tables.

- **Pipes:** each pipe maps a bucket + prefix + file pattern to a table, e.g.
  `s3://home/raw/orders/*.csv` → `lake.raw.orders`.
- **Formats:** CSV, JSON / NDJSON and Parquet, optionally `.gz` or `.zst`.
  The format comes from the file extension, or is set per pipe.
- **Exactly once:** the list of loaded files is a table in the lake
  (`lake.duckpipe.load_history`), written in the same transaction as the data.
  A crash can't leave a file half-loaded or loaded twice.
- **Load history:** like Snowpipe's `COPY_HISTORY`, every file loaded or
  failed is kept with its pipe, target table, rows, status and timestamp:
  `duckpipe history [pipe]`, or query the table (see below).
- **Changed files:** a file overwritten with new content (new ETag) is loaded
  again, as Snowpipe does. Its old rows stay; use `_file` to tell them apart.
- **Schema:** every table has the same three columns, as Snowflake keeps raw
  data in a `VARIANT`: `record` (JSON, one row of the file as an object keyed
  by column name), `_file` (its `s3://` URL) and `_loaded_at`. A file with
  new, missing or differently typed columns never fails on the table; you
  pick the fields out later (e.g. in dbt): `record->>'price'`.
- **Partitions:** target tables are partitioned by the UTC day of `_loaded_at`
  (`…/year=2026/month=10/day=20/*.parquet`), so a filter on `_loaded_at` only
  reads those days' files. Tables from before this get the partitioning on
  their next load; rows already there stay where they are. A table you
  partition differently yourself is left alone.
- **Bad files:** a file that can't be loaded (wrong types, broken CSV) is
  marked failed and skipped; the others keep loading. Fix it (upload a new
  version) or fix the pipe and run `retry`. Network or S3 outages don't mark
  anything failed; the next poll just tries again.
- **Batches:** new files are loaded oldest first, up to 50 per transaction. If
  a batch fails, its files are loaded one by one, so only the bad ones fail.
- **Housekeeping:** small loads are first kept in the catalog, then written
  out and merged into larger Parquet files every 6 hours. Old snapshots are
  dropped after 30 days.

- **Monitoring:** loads and failures are published to [relay](../relay) when
  it is installed, and vigil reads them from there (see below).
- **Page:** a web page shows what is loading, the files waiting and the load
  history, and can stop all loading (see [The page](#the-page)).

Garage can't send notifications when a file arrives, so duckpipe lists each
prefix every 30 seconds (`poll`). `sudo systemctl kill -s HUP duckpipe` makes it
look right away.

## Files

| File                     | What it is                                                                    |
|--------------------------|-------------------------------------------------------------------------------|
| `main.go`                | Commands and the polling loop                                                 |
| `engine.go`              | Listing S3, loading files, the load history, status, retry, maintenance       |
| `duck.go`                | Runs SQL scripts with the `duckdb` CLI and reads their results                |
| `config.go`              | The config file                                                               |
| `ui.go`                  | The web page's server: JSON API, stop all, resume                             |
| `ui.html`                | The page (plain HTML and JavaScript, built into the binary)                   |
| `duckpipe.yaml`          | The server's config (the pipes), with every setting explained. Deployed as is |
| `deploy.sh`              | Run on your Mac: builds the binary and installs it on the server              |
| `install.sh`             | Run on the server by `deploy.sh`: installs duckpipe, the DuckDB CLI, key, config and service |
| `undeploy.sh`            | Run on your Mac: removes duckpipe from the server                             |

## Config

The server's config is `duckpipe.yaml` in this directory: change it here and
deploy (see [Deploy](#deploy-to-the-server)). A small config:

```yaml
lake:
  catalog: ducklake:sqlite:/var/lib/duckpipe/catalog.sqlite
  data_path: s3://lake/
poll: 30s
pipes:
  - name: orders
    bucket: home
    prefix: raw/orders/
    pattern: "*.csv"
    table: raw.orders
    options: "delim = ';', all_varchar = true"   # optional, passed to read_csv
```

| Pipe setting | Default            | Meaning                                                                         |
|--------------|--------------------|---------------------------------------------------------------------------------|
| `name`       | required           | Name in logs, `status` and the load history. Don't rename a pipe: it would load everything again |
| `bucket`     | required           | Bucket to watch                                                                 |
| `prefix`     | whole bucket       | Folder to watch, including subfolders                                           |
| `pattern`    | every file         | Glob on the file name (`*.csv`). With a `/`, on the path below the prefix (`*/orders-*.csv`) |
| `format`     | from extension     | `csv`, `json` or `parquet`                                                      |
| `table`      | `main.<name>`      | Target table, `[schema.]table`                                                  |
| `options`    |                    | Extra options for `read_csv` / `read_json` / `read_parquet`, as SQL             |
| `batch`      | `50`               | Most files per transaction                                                      |

Column names and types are detected from each file and kept in `record`: a
CSV number becomes a JSON number, text a JSON string. `options: "all_varchar =
true"` keeps every CSV value a string, so the same field has the same JSON type
in every file. JSON files are read with `read_json_objects`, so each object
goes into `record` as it is; their `options` go to that function.

Pick the catalog by who needs to read the tables:

| `catalog`                              | Who can query while duckpipe runs                        |
|----------------------------------------|----------------------------------------------------------|
| `ducklake:sqlite:/path/catalog.sqlite` | Anyone on the server who can read the file               |
| `ducklake:postgres:dbname=lake host=…` | Any machine that can reach Postgres (the server's)       |
| `ducklake:/path/catalog.ducklake`      | Nobody else: DuckDB locks the file                       |

## Query the tables

The easiest way is [lake-ui](../lake-ui). From DuckDB **1.5 or newer** (older
versions can't read this DuckLake format), on any machine that can reach the
server's Postgres, with the catalog login and an S3 key from
`/etc/ducklake.env` (the catalog's password in `PGPASSWORD`):

```sql
CREATE SECRET (TYPE s3, KEY_ID 'GK…', SECRET '…', REGION 'garage',
               ENDPOINT '192.168.1.169:3900', URL_STYLE 'path', USE_SSL false);
ATTACH 'ducklake:postgres:dbname=ducklake host=192.168.1.169 user=ducklake' AS lake (READ_ONLY);

FROM lake.raw.orders LIMIT 10;
SELECT record->>'order_id' AS order_id, (record->>'amount')::DECIMAL(10, 2) AS amount, _file
FROM lake.raw.orders;
FROM lake.duckpipe.load_history ORDER BY loaded_at DESC;   -- what was loaded, into which table, when
FROM lake.snapshots();                                     -- one per load
FROM lake.raw.orders AT (VERSION => 3);                    -- time travel
```

## Run it locally

```sh
go build -o duckpipe .
./duckpipe -config my.yaml once      # one pass, then exit (exit code 1 if a pipe couldn't be polled)
./duckpipe -config my.yaml run       # keep watching
./duckpipe -config my.yaml status    # files and rows per pipe, failed files
./duckpipe -config my.yaml retry [pipe]
./duckpipe -config my.yaml history [pipe]   # every file loaded or failed, newest first
./duckpipe -config my.yaml check     # check the config only; touches nothing
```

`load_history` has one row per load attempt:

| Column          | Meaning                                              |
|-----------------|------------------------------------------------------|
| `pipe`          | Pipe that loaded it                                  |
| `bucket`, `key` | The file                                             |
| `etag`, `size`, `last_modified` | The file's version when it was loaded |
| `target`        | Table it went into, `lake.schema.table`              |
| `status`        | `loaded` or `failed`                                 |
| `rows`          | Rows loaded                                          |
| `error`         | Why it failed                                        |
| `loaded_at`     | When                                                 |

`retry` deletes the `failed` rows, so those attempts leave the history.

You need the `duckdb` CLI, version 1.5 or newer, on your `PATH` (or pass
`-duckdb`): `curl https://install.duckdb.org | sh`. For a local catalog, use
e.g. `catalog: ducklake:sqlite:./catalog.sqlite`.

Credentials come from the usual AWS environment variables or your `~/.aws`
profile, as for the other tools:

```sh
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
AWS_DEFAULT_REGION=garage                    # default: garage
AWS_ENDPOINT_URL=http://192.168.1.169:3900   # this is the default
```

| Flag            | Default                                               | Meaning                                       |
|-----------------|-------------------------------------------------------|-----------------------------------------------|
| `-config`       | `$DUCKPIPE_CONFIG`, else `duckpipe.yaml`              | Config file                                   |
| `-duckdb`       | `$DUCKPIPE_DUCKDB`, else `duckdb` from `PATH`         | DuckDB CLI to run loads with (1.5 or newer)   |
| `-endpoint-url` | `$AWS_ENDPOINT_URL`, else `http://192.168.1.169:3900` | S3 endpoint                                   |
| `-region`       | from env or profile, else `garage`                    | S3 region                                     |
| `-profile`      |                                                       | Profile name from `~/.aws`                    |
| `-state-dir`    | `$DUCKPIPE_STATE_DIR`, else DuckDB's own (`~/.duckdb`) | Where DuckDB keeps extensions and spill files |
| `-listen`       | `127.0.0.1:8100`                                      | Address of the page (`run` only); empty for none |
| `-auth`         | `$DUCKPIPE_UI_AUTH`                                   | Basic auth for the page, as `user:pass`; empty for none |

DuckDB downloads its `httpfs`, `ducklake` and `sqlite` extensions on first use,
so the first run needs internet access.

## Deploy to the server

```sh
./deploy.sh
```

You need Go on your Mac, SSH access to the server and `sudo` there. Like the
other tools, `deploy.sh` cross-compiles a static Linux binary, copies it over
with `install.sh` and runs that with `sudo`.

| Variable         | Default                 | Meaning                                                    |
|------------------|-------------------------|------------------------------------------------------------|
| `HOST`           | `192.168.1.169`         | SSH destination                                            |
| `SSH_PORT`       | `2222`                  | SSH port on the server                                     |
| `SOURCE_BUCKETS` | `home`                  | Buckets duckpipe may read raw files from (separated by spaces) |
| `LAKE_BUCKET`    | `lake`                  | Bucket the tables are written to; created if missing       |
| `KEY_NAME`       | `duckpipe-key`          | Name of the Garage key duckpipe uses                       |
| `ENDPOINT`       | `http://127.0.0.1:3900` | S3 endpoint, as seen from the server                       |
| `DUCKDB_VERSION` | `v1.5.6`                | DuckDB CLI release to install (1.5 or newer)               |
| `LISTEN`         | `[::]:8100`             | The page; `127.0.0.1:8100` for local only                  |
| `UI_USER`        | `admin`                 | Basic-auth user for the page (first install only)          |

### What `install.sh` does (on the server)

1. Stops early if Garage isn't installed and running.
2. Downloads the DuckDB CLI from its GitHub release to `/usr/local/bin/duckdb`
   (skipped if that version is already there), and installs
   `/usr/local/bin/duckpipe` and the `duckpipectl` helper.
3. Creates a `duckpipe` system user, which owns the catalog in
   `/var/lib/duckpipe`.
4. Creates the lake bucket and a key, `duckpipe-key`, that can only **read**
   the source buckets and read/write the lake bucket.
5. Installs `duckpipe.yaml` from the repo as `/etc/duckpipe.yaml`, every
   time. It is checked first, with `duckpipe check`, on your Mac and again on
   the server: if it doesn't load, the deploy stops before anything is
   replaced and duckpipe keeps running as it was. On the first run only, it
   writes `/etc/duckpipe.env` (the key, root only); later runs leave it
   alone, except to add a login for the page (`DUCKPIPE_UI_AUTH`) if there
   isn't one.
6. Writes `/etc/systemd/system/duckpipe.service` with the same hardening as
   `garage.service`, then enables and restarts it.
7. Waits until duckpipe is watching and the page answers. If it doesn't
   start, it shows the log.

## After deploying

- **Change your pipes:** edit `duckpipe.yaml` here and deploy
  (`ONLY=duckpipe ../deploy-all.sh`). Don't edit `/etc/duckpipe.yaml` on the
  server: the next deploy replaces it. A pipe's bucket must be one of
  `SOURCE_BUCKETS`, or duckpipe's key can't read it.
- **Page:** `http://<server>:8100`; login: `sudo grep DUCKPIPE_UI_AUTH /etc/duckpipe.env`
- **Status:** `sudo duckpipectl status`
- **Retry failed files:** `sudo duckpipectl retry [pipe]`
- **Load history:** `sudo duckpipectl history [pipe]`
- **Poll now:** `sudo systemctl kill -s HUP duckpipe`
- **Logs:** `journalctl -u duckpipe -f`
- **New source bucket:** re-run with the full list:
  `SOURCE_BUCKETS="home newbucket" ./deploy.sh`

Use `duckpipectl` rather than running `duckpipe` as root: it runs as the
service's user with the service's key, so it can't leave root-owned catalog
files behind that the service can't open.

## The page

`duckpipe run` serves a page on `-listen` (port 8100 on the server, behind a
login), also linked from [home](../home):

- **Loading:** the batch being loaded now: pipe, table, its files, size and
  how long it has been running. Maintenance shows too while it runs.
- **Pipes:** per pipe, the files the last poll found that aren't loaded yet
  (waiting), the files and rows loaded, the failed files, the last load, and
  the last poll, with its error if it failed.
- **History:** the newest 500 rows of the load history, for all pipes or
  one, with the error of each failed file. The rest:
  `sudo duckpipectl history [pipe]`.
- **Stop all:** stops the load under way right away (its `duckdb` process is
  killed, so its transaction is rolled back and nothing of it is kept) and
  stops polling until **Resume**. The stopped files aren't marked failed:
  they are loaded again after Resume. Maintenance under way is stopped too.
  A stopped load is published to relay as `failed`, with the error
  `stopped from the page`, so vigil shows that it was stopped.

Being stopped is kept in memory only: restarting the service (or a deploy)
resumes loading. The page refreshes itself every few seconds. Pipes come from
`duckpipe.yaml` and can't be changed here.

## relay and vigil

When `RELAY_URL` is set or `/etc/relay-client.env` exists (relay's
`install.sh` writes it; the service reads it), duckpipe publishes to relay.
[vigil](../vigil) reads these events from relay as app `duckpipe`:

| Topic             | Type      | When                                                              |
|-------------------|-----------|-------------------------------------------------------------------|
| `duckpipe.ingest` | `started` | A batch starts loading                                            |
| `duckpipe.ingest` | `ok`      | It is loaded, with `items` (rows) and `duration_ms`               |
| `duckpipe.ingest` | `failed`  | It failed, with `error`. A failed batch of several files is then retried one file at a time, each with its own events |
| `duckpipe.poll`   | `failed`  | A pipe (or the load history) can't be read, e.g. S3 or catalog down |
| `duckpipe.poll`   | `ok`      | It can be read again                                              |

`ingest` events share a `run_id` per batch, and their `data` has `name`
(`pipe: file -> table`; for a batch of several files,
`pipe: 50 files from s3://bucket/prefix/ (first … last) -> table`), `pipe`,
`table`, `files` and `bytes`. `poll`
events are sent when the state changes, not on every poll, so an outage is
one event. Each failure raises a vigil alert. For an alert when nothing has
loaded for a while, deploy vigil with e.g. `EXPECT="duckpipe/ingest=840h"`
(35 days, for monthly files). Without relay, nothing is sent.

## Limits

- Polling lists the whole prefix every time. That's fine up to some hundreds of
  thousands of files per pipe. Beyond that, move processed files elsewhere or
  split the prefix into several pipes.
- Keys containing `*`, `?`, `[` or `{` are marked failed, because DuckDB would
  treat them as wildcards.
- A table from before the `record` layout (with typed columns) isn't loaded
  into: its pipe shows an error until you drop it and clear its history, so
  its files load again (see below).
- Run one duckpipe per catalog.

### Reload a table

To start a pipe's table over (e.g. one from before the `record` layout), drop
it and its load history in one go; the next poll loads every file again:

```sh
# As the service, with its catalog login (PG* variables in /etc/duckpipe.env).
sudo systemd-run --quiet --pipe --wait --collect -p EnvironmentFile=/etc/duckpipe.env \
  -p User=duckpipe -p Environment=HOME=/var/lib/duckpipe duckdb -c "
  ATTACH 'ducklake:postgres:dbname=ducklake host=127.0.0.1 user=ducklake' AS lake;
  BEGIN; DROP TABLE lake.uk.crime_street; DELETE FROM lake.duckpipe.load_history WHERE pipe = 'crime_street'; COMMIT;"
sudo systemctl kill -s HUP duckpipe
```

The dropped table's Parquet files are deleted by maintenance once its
snapshots expire (`keep_snapshots`).

## Uninstall

```sh
./undeploy.sh            # keeps /etc/duckpipe.yaml, the catalog and the lake bucket
PURGE=1 ./undeploy.sh    # also deletes the config, the catalog and the duckpipe user
```

It stops the service and removes the binaries (including `duckdb`), the unit,
`/etc/duckpipe.env` and the S3 key. The lake bucket is never touched. Takes
`HOST`, `SSH_PORT` and `KEY_NAME` like `deploy.sh`.
