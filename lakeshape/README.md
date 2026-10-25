# lakeshape

dbt-style SQL models for the DuckLake set up by
`../linux-scripts/ducklake-setup.sh` (the tables duckpipe loads). Models are
`.sql` files with Jinja templates; their `ref()` and `source()` calls make
the DAG they are built in. The same Go binary is the CLI, for building in
your own dev schemas, and a service that builds prod on a schedule or when
[relay](../relay) events arrive (e.g. "duckpipe loaded `raw.orders`"). The
design and its reasons are in [SPEC.md](SPEC.md).

- **Models:** `view`, `table` or `incremental` (`append`, or `delete+insert`
  on a `unique_key`), with `is_incremental()` and `{{ this }}`.
- **Macros:** `{% macro %}` in `macros/*.sql`, available in every model.
- **Tests:** `not_null`, `unique`, `accepted_values` and `relationships` on
  columns, and singular tests in `tests/`. They run **inside the model's
  transaction, before the commit**: if an `error` test fails, the model rolls
  back, its previous version stays, and everything downstream is skipped.
  `warn` tests only report.
- **Selection:** `orders`, `+orders`, `orders+`, `tag:daily`,
  `path:models/staging`, `source:raw.orders+`, and `-exclude`.
- **Dev and prod:** the CLI builds into `dev_<user>__<schema>` and reads
  sources from prod; only the service writes prod.
- **Jobs:** cron schedules and relay triggers, with debounce, one run at a
  time, and events acknowledged only after their run.
- **History:** every model and test of every run in
  `lake.lakeshape.run_history`, with the DuckLake snapshot each model
  committed.

Like duckpipe and lake-ui, it runs SQL with the official `duckdb` CLI (1.5 or
newer), so it needs no cgo.

## Files

| File                | What it is                                                                 |
|---------------------|----------------------------------------------------------------------------|
| `main.go`           | Commands and flags                                                         |
| `project.go`        | Loading `project.yaml`, `sources.yml`, models and macros; config; targets  |
| `render.go`         | The Jinja renderer: `ref`, `source`, `config`, `var`, `is_incremental`, `this`, `target`, macros |
| `dag.go`            | Dependency order and cycle detection                                       |
| `select.go`         | `-select` and `-exclude`                                                   |
| `tests.go`          | Column tests and singular tests                                            |
| `build.go`          | Running models and tests in DAG order                                      |
| `history.go`        | `run_history`, `history`, `ls -orphans`                                    |
| `lake.go`, `duck.go`| Attaching the lake; running SQL scripts with the `duckdb` CLI              |
| `jobs.go`           | `jobs.yml`: schedules and relay triggers                                   |
| `serve.go`          | The service, and `trigger`                                                 |
| `relay.go`          | Reading and publishing relay events                                        |
| `mcp.go`            | The MCP tools on `-listen`                                                 |
| `SPEC-mcp.md`       | The MCP tools' spec                                                        |
| `testdata/shop/`    | An example project, used by the tests                                      |
| `deploy.sh`         | Run on your Mac: builds the binary and installs it with your project on the server |
| `install.sh`        | Run on the server by `deploy.sh`: DuckDB CLI, user, key, project, service  |
| `undeploy.sh`       | Run on your Mac: removes lakeshape from the server                         |

## A project

```
project.yaml          # name, lake, default schema, time zone, threads, vars, per-folder config
sources.yml           # the lake tables models read with source()
jobs.yml              # what the service runs, and when
models/**/*.sql       # one model per file; the file name is the model name
models/**/*.yml       # column tests
macros/*.sql          # {% macro %} definitions
tests/*.sql           # singular tests: fail if they return rows
```

`testdata/shop/` has one of each.

```yaml
# project.yaml
name: shop
lake:
  catalog: ducklake:postgres:dbname=ducklake   # host, user, password from PG* env vars
  data_path: s3://lake/
schema: analytics      # default schema for models (default: main)
timezone: UTC          # for SQL and job schedules (default: UTC)
threads: 4             # models built at once (default: 4; use 1 with an SQLite catalog)
vars:
  min_amount: 0
models:                # config by folder under models/; deeper folders win, tags add up
  staging: {schema: staging, materialized: view, tags: [staging]}
  marts: {schema: marts, materialized: table, tags: daily}
  marts/finance: {schema: finance}
```

```yaml
# sources.yml
sources:
  - name: raw          # source('raw', 'orders') is raw.orders
    tables: [orders, customers]
```

A model:

```sql
{{ config(materialized='incremental', unique_key='order_id') }}
select o.*, c.name as customer
from {{ ref('stg_orders') }} o
join {{ ref('stg_customers') }} c using (customer_id)
{% if is_incremental() %}
where o.ordered_at > (select max(ordered_at) from {{ this }})
{% endif %}
```

Its tests:

```yaml
# models/marts/orders.yml
models:
  - name: orders
    columns:
      - name: order_id
        tests: [not_null, unique]
      - name: status
        tests:
          - accepted_values: {values: [placed, shipped, returned]}
      - name: customer_id
        tests:
          - relationships: {to: "ref('stg_customers')", field: customer_id, severity: warn}
```

| In a template       | Meaning                                                                   |
|---------------------|---------------------------------------------------------------------------|
| `ref('m')`          | Model `m`'s relation, `schema.table`, in the current target               |
| `source('s', 't')`  | A table declared in `sources.yml`. Always prod                            |
| `config(...)`       | `materialized`, `schema`, `unique_key`, `incremental_strategy`, `tags`    |
| `var('k', default)` | From `project.yaml` `vars`, or `-vars`                                    |
| `is_incremental()`  | True when an incremental model's table exists, without `-full-refresh`    |
| `this`              | The model's own relation                                                  |
| `target.name`       | `dev` or `prod`                                                           |

`ref()` and `this` are strings, not objects. An undefined name is an error.
Every problem in a project (a missing ref, an undeclared source, a cycle, a
template error with its line, a broken `jobs.yml`) is reported when it
loads, before anything runs.

## Run it on your Mac

```sh
go build -o lakeshape .
cd ../analytics                     # your project
lakeshape ls                        # models in DAG order: relation, materialization, tags, dependencies
lakeshape compile -select orders    # the SQL that would run
lakeshape run                       # build everything into dev_<you>__*
lakeshape run -select 'stg_orders+' # one model and what depends on it
lakeshape test -select tag:daily    # tests only, against what is built
lakeshape history orders            # the model's (and its tests') latest runs
lakeshape ls -target prod           # what prod looks like
lakeshape ls -orphans               # tables in your schemas that no model builds
```

```
run 3dd20fe01dc61dda (dev): 5 models, 4 threads
  success  dev_nicolas__staging.stg_orders          view          170ms
  success  dev_nicolas__staging.stg_customers       view          280ms
  success  dev_nicolas__marts.recent                view          160ms
  warn     dev_nicolas__marts.orders                incremental   320ms  4 rows
    warn   test relationships_orders_customer_id: 1 failing rows
  success  dev_nicolas__finance.revenue             table         140ms  3 rows
  success  dev_nicolas__finance.revenue,dev_nicolas__marts.orders test revenue_matches_orders  120ms
done in 920ms: 5 ok, 1 warn, 0 error, 0 skipped
```

In dev, every `ref()` points at your dev schemas. A model whose upstream you
haven't built in dev fails before anything runs, with the models to add
(`+model` builds them too). Sources are read from prod. lakeshape refuses to
write any schema that isn't yours (`dev_<user>__`), but that is a check in
lakeshape, not a permission: the credentials you use can write the lake.

A flag the command doesn't use is an error rather than ignored (for example
`-full-refresh` on `compile`).

`run` and `test` exit 1 if a model or an `error` test failed or a model was
skipped, and 2 if the project doesn't load.

| Flag            | Default                                               | Meaning                                         |
|-----------------|-------------------------------------------------------|-------------------------------------------------|
| `-project`      | `$LAKESHAPE_PROJECT`, else `.`                        | Project directory                               |
| `-select`       | every model                                           | Models to use, e.g. `'stg_orders+ tag:daily'`   |
| `-exclude`      |                                                       | Models to leave out, same syntax                |
| `-full-refresh` |                                                       | `run`, `trigger`: build incremental models from scratch |
| `-vars`         |                                                       | JSON object overriding `project.yaml` `vars`    |
| `-target`       | `dev`                                                 | `prod` for `compile` and `ls` only              |
| `-dev-user`     | `$USER`                                               | Dev schemas are `dev_<user>__<schema>`          |
| `-duckdb`       | `$LAKESHAPE_DUCKDB`, else `duckdb` from `PATH`        | DuckDB CLI (1.5 or newer)                       |
| `-endpoint-url` | `$AWS_ENDPOINT_URL`, else `http://192.168.1.169:3900` | S3 endpoint                                     |
| `-region`       | from env or profile, else `garage`                    | S3 region                                       |
| `-profile`      |                                                       | Profile name from `~/.aws`                      |
| `-state-dir`    | `$LAKESHAPE_STATE_DIR`, else DuckDB's own             | Where DuckDB keeps extensions and spill files   |
| `-listen`       |                                                       | `serve`: address of the MCP tools (`/mcp`, behind `$LAKESHAPE_MCP_TOKEN`); empty for none |

Credentials come from the environment, like the other tools: the usual AWS
variables (or `~/.aws`) for the lake's data, and `PG*` variables for a
Postgres catalog. With the values from `/etc/ducklake.env` on the server:

```sh
export AWS_ACCESS_KEY_ID=GK… AWS_SECRET_ACCESS_KEY=… AWS_ENDPOINT_URL=http://192.168.1.169:3900
export PGHOST=192.168.1.169 PGUSER=ducklake PGPASSWORD=…
```

DuckDB downloads its `ducklake`, `postgres` (or `sqlite`) and `httpfs`
extensions on first use, so the first run needs internet access.

## Jobs

```yaml
# jobs.yml
jobs:
  - name: orders
    select: source:raw.orders+       # what to build
    on:                              # relay events that start it
      - topic: duckpipe.ingest       # relay topic filter: a topic, prefix.*, or a comma list
        type: ok                     # one type or a comma list; empty: any
        data: {table: lake.raw.orders}   # top-level data fields that must be equal
    debounce: 60s                    # run once events have been quiet this long (default 60s)
    max_wait: 10m                    # or this long after the first one (default 10m)
  - name: nightly
    select: tag:daily
    schedule: "0 3 * * *"            # 5-field cron, in the project's time zone
```

A job takes `exclude`, `full_refresh: true` and `vars` too. The service runs
one run at a time. Jobs that become due together share a run. A job
triggered during a run runs once more afterwards, however many events came.
A schedule missed while the service was down runs once when it starts.

## Deploy to the server

```sh
./deploy.sh                          # deploys ../analytics
PROJECT=~/other-project ./deploy.sh
```

You need Go on your Mac, SSH access to the server and `sudo` there, and
`ducklake-setup.sh` run on the server first. `deploy.sh` checks that the
project loads, cross-compiles a static Linux binary, copies it with
`install.sh` and the project (without dot files such as `.git`), and runs
`install.sh` with `sudo`. Re-run it to ship a new project or version: a new
binary restarts the service (a run in progress rolls back), a new project
only reloads it, between runs.

| Variable         | Default                 | Meaning                                              |
|------------------|-------------------------|------------------------------------------------------|
| `PROJECT`        | `../analytics`          | Your project directory (default: next to `lakeshape/`) |
| `HOST`           | `192.168.1.169`         | SSH destination                                      |
| `SSH_PORT`       | `2222`                  | SSH port on the server                               |
| `LAKE_BUCKET`    | `lake`                  | Bucket the lake's data is in                         |
| `KEY_NAME`       | `lakeshape-key`         | Name of the Garage key lakeshape uses                |
| `ENDPOINT`       | `http://127.0.0.1:3900` | S3 endpoint, as seen from the server                 |
| `DUCKDB_VERSION` | `v1.5.6`                | DuckDB CLI release to install if missing             |

### What `install.sh` does (on the server)

1. Stops early if Garage isn't running or `/etc/ducklake.env` is missing.
2. Installs the DuckDB CLI if that version isn't there, `/usr/local/bin/lakeshape`
   and the `lakeshapectl` helper.
3. Creates a `lakeshape` system user, and a key, `lakeshape-key`, that can
   read and write the lake bucket.
4. On the first run only, writes `/etc/lakeshape.env` (root only): that key,
   and the catalog's `PG*` settings from `/etc/ducklake.env`.
5. Unpacks the project into `/etc/lakeshape/project` (readable by everyone:
   it holds no secrets). If it doesn't load
   there, the previous project is put back and nothing else changes.
6. Writes `/etc/systemd/system/lakeshape.service` with the same hardening as
   the other services, then starts, restarts or reloads it as needed.

## After deploying

- **Run a job now:** `sudo lakeshapectl trigger nightly` (add
  `-full-refresh` to rebuild incremental models). It asks the service through
  relay, follows the run and exits 0 or 1 with it. A job always runs in
  prod, so `-dev-user`, `-target` and `-select` are refused; to build a
  job's models in dev, run them from your Mac with `lakeshape run -select …`.
- **History:** `sudo lakeshapectl history [model]`
- **Prod models:** `sudo lakeshapectl ls -target prod`
- **Logs:** `journalctl -u lakeshape -f`
- **Reload the project by hand:** `sudo systemctl reload lakeshape` (a
  project that doesn't load is refused, and the old one keeps running)

Use `lakeshapectl` rather than running `lakeshape` as root: it runs as the
service's user, with its credentials and the deployed project.

## MCP

The service serves two tools at `/mcp` on `-listen` (`127.0.0.1:8103` on
the server), behind `Authorization: Bearer $LAKESHAPE_MCP_TOKEN`. Agents
reach them through mcp-hub as `lakeshape__*`; the `nexus` profile has both.

| Tool        | Read-only | What it does                                                     |
|-------------|-----------|------------------------------------------------------------------|
| `list_jobs` | yes       | The loaded jobs: `select`, `exclude`, schedule, `on:` triggers, last run, and whether each is running now |
| `run_job`   | no        | `{job, full_refresh?}`: what `trigger` does; returns `{run_id, topic: "lakeshape.run"}` at once. `_meta.wait` is `2h` |

Details in [SPEC-mcp.md](SPEC-mcp.md).

## relay and vigil

The service reads its triggers through a relay consumer named `lakeshape`.
It reads ahead, but moves the consumer's cursor past an event only once the
run it triggered has finished, so after a crash the event is read again and
the job runs. Without relay (no `RELAY_URL` and no `/etc/relay-client.env`),
schedules still work and `on:` triggers never fire.

The service publishes, and vigil alerts on every `failed`:

| Topic               | Type                              | When / data                                                  |
|---------------------|-----------------------------------|--------------------------------------------------------------|
| `lakeshape.run`     | `started`, `ok`, `failed`         | Per run: `jobs`, `models`, `duration_ms`, `error`            |
| `lakeshape.model`   | `ok`, `warn`, `failed`, `skipped` | Per model: `model`, `relation`, `materialized`, `rows`, `duration_ms`, `error`, failed `tests` |
| `lakeshape.project` | `failed`, `ok`                    | A reload was refused, or worked again after a refusal        |

Every event of a run shares its `run_id`. For an alert when nothing has run
for a while, deploy vigil with e.g. `EXPECT="lakeshape/run=26h"`. CLI runs
on your Mac publish nothing.

## Limits

- One run at a time, by design. Within a run, up to `threads` models build
  at once.
- An SQLite catalog lets only one writer commit at a time: use `threads: 1`
  with one (conflicting commits are retried 3 times). The Postgres catalog
  from `ducklake-setup.sh` has no such limit.
- A test on several models can't roll them back, because each model commits
  on its own; it can only stop what is downstream of them.
- lakeshape never drops a table whose model was deleted: `ls -orphans` lists
  them.
- Not in v1: ephemeral models, seeds, snapshots, packages, hooks, a docs
  site, source freshness, tests on sources.

## Tests

```sh
go test ./...
LAKESHAPE_TEST_DUCKDB=/path/to/duckdb go test ./...   # also builds the example against a local lake (DuckDB 1.5+)
```

## Uninstall

```sh
./undeploy.sh            # keeps the deployed project, the user, and everything in the lake
PURGE=1 ./undeploy.sh    # also deletes /var/lib/lakeshape and the lakeshape user
```

It stops the service and removes the binaries, the unit, `/etc/lakeshape.env`
and the S3 key. The tables lakeshape built, and its run history, stay in the
lake. Takes `HOST`, `SSH_PORT` and `KEY_NAME` like `deploy.sh`.
