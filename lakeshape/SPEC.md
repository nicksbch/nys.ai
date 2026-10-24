# lakeshape: spec (approved 2026-10-20)

A dbt-style transformation engine for the DuckLake set up by
`../linux-scripts/ducklake-setup.sh`. It reads a folder of SQL models written
with Jinja templates, builds a DAG from their `ref()` and `source()` calls,
and builds each model into the lake in dependency order. The same Go binary
is the CLI (for development) and a service that runs jobs on a schedule or
when [relay](../relay) events arrive.

Like duckpipe and lake-ui, it runs SQL through the official `duckdb` CLI
(1.5 or newer, the same `DUCKDB_VERSION`), so it needs no cgo.

## What it is not

- **Not dbt-compatible.** The project looks like a dbt project (same
  folders, same `ref`/`source`/`config` calls), but dbt projects and packages
  (`dbt_utils`, adapter dispatch, `profiles.yml`) are not expected to run
  unchanged.
- **No SQL parsing.** The DAG comes from what the templates call while they
  render, as in dbt. lakeshape never parses the SQL it sends to DuckDB.
- **No database access while rendering.** There is no `run_query`,
  `adapter.*` or introspection, so a project compiles the same way without a
  connection, and the DAG never depends on data.

## Project

```
project.yaml          # name, lake, default schema, vars, per-folder model config
sources.yml           # sources: names for the lake tables duckpipe loads
jobs.yml              # what the service runs, and when
models/**/*.sql       # one model per file
models/**/*.yml       # column tests
macros/*.sql          # {% macro %} definitions, available in every model
tests/*.sql           # singular tests
```

```yaml
# project.yaml
name: analytics
lake:
  catalog: ducklake:postgres:dbname=ducklake   # host, user, password from PG* env vars
  data_path: s3://lake/
schema: analytics            # default schema for models
timezone: UTC                # for job schedules
threads: 4
vars:
  start_date: "2020-01-01"
models:                      # config by folder under models/; deeper folders win
  staging: {schema: staging, materialized: view}
  marts:   {schema: marts,   materialized: table, tags: [daily]}
```

```yaml
# sources.yml
sources:
  - name: raw                # source('raw', 'orders') → raw.orders
    schema: raw              # default: the source name
    tables: [orders, events]
```

- A model's name is its file name without `.sql`. It must be unique across
  `models/`, because `ref()` takes only the name.
- Config is resolved in this order (first one wins): `config()` in the
  model, then the deepest folder entry in `project.yaml`, then the project
  defaults (`schema`, default `main`; `materialized: view`). Tags are not
  replaced but added up, from every folder level and `config()`.
- `incremental_strategy` defaults to `delete+insert` when the model has a
  `unique_key`, else `append`.
- A folder in `project.yaml` `models:` must exist under `models/`, so a typo
  is an error, not silently ignored config.
- Model schemas may not be `lakeshape` (lakeshape's own tables) or start
  with `dev_` (dev targets). A model may not be built where a declared
  source table is.
- `macros/*.sql` files hold `{% macro %}` definitions. Every macro is
  available in every model, and macros can call `ref()`, `source()` and each
  other. A macro name may be defined only once, and may not be one of the
  names below.

## Templating

[gonja](https://github.com/nikolalohinski/gonja) (Jinja syntax): `{{ }}`,
`{% if %}`, `{% for %}`, `{% set %}`, `{% macro %}`, filters, comments.

| Name                  | Meaning                                                                    |
|-----------------------|----------------------------------------------------------------------------|
| `ref('m')`            | The relation of model `m` in the current target. Adds the edge `m → this model` |
| `source('s', 't')`    | The relation of a declared source table. Always prod, in every target      |
| `config(...)`         | `materialized`, `schema`, `unique_key`, `incremental_strategy`, `tags`     |
| `var('k', default)`   | From `project.yaml` `vars`, overridden by `-vars '{"k": …}'`              |
| `is_incremental()`    | True when the model is incremental, its table exists, and this is not `-full-refresh` |
| `this`                | This model's own relation                                                  |
| `target.name`         | `prod` or `dev`                                                            |

- Relations are written `schema.table`, with no catalog prefix. Every
  script starts with `USE lake`. Views stored in the lake then work whatever
  name a reader attaches it as.
- `ref()` and `this` are strings (`schema.table`), not objects: there is
  no `ref('x').schema`.
- An undefined name is an error, not an empty string.
- Each model is rendered in two phases. The **parse** phase finds its
  config and dependencies. In it, `ref('x')` and `this` give just the model
  name, because a model's schema is only known once every model is parsed.
  An incremental model is parsed twice, with `is_incremental()` false and
  true, and depends on what either render refs. Its `config()` must be the
  same in both. The **compile** phase gives the SQL to run. If its
  `config()`, `ref()` or `source()` calls differ from the parse phase (they
  depended on the value of `ref()` or `this`), the model fails.
- These fail at compile time, before anything runs: a template error, a
  missing model or source, a cycle, a duplicate model name, an unknown
  config key, and an `incremental` model without `unique_key` when its
  strategy needs one.

## Materializations

| `materialized` | What runs (one transaction per model)                                     |
|----------------|---------------------------------------------------------------------------|
| `view`         | `CREATE OR REPLACE VIEW this AS <sql>`                                    |
| `table`        | `CREATE OR REPLACE TABLE this AS <sql>`. Readers see the old version or the new one, never neither |
| `incremental`  | The first run, and every `-full-refresh`, builds it as a `table`. Later runs: |
|                | `append`: `INSERT INTO this <sql>`                                       |
|                | `delete+insert` (needs `unique_key`, one or more columns): the new rows go to a temp table; rows of `this` with those keys are deleted; the new rows are inserted |

- The model's SQL is put inside `SELECT * FROM (…)`, so it must be one
  query; a trailing `;` is dropped.
- The schema is created if it is missing. Schemas are created one after
  another before the run starts, so parallel models don't collide.
- If a model changes materialization (view ↔ table), the old relation is
  dropped in the same transaction.
- **Incremental schema changes fail.** If the new rows' columns (names or
  types) differ from the table's, the model fails with a message to run
  `-full-refresh`.
- lakeshape never drops a table whose model was deleted. `ls -orphans`
  lists such tables.

## Tests

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
          - relationships: {to: "ref('customers')", field: id, severity: warn}
```

- Built-in tests: `not_null`, `unique`, `accepted_values`, `relationships`.
  Each one is a query that returns the bad rows. It passes when there are
  none. Their ids are `<test>_<model>_<column>`. `description:` fields are
  allowed and ignored.
- A `relationships` test against another model makes the model build after
  that one (without it being a `ref`).
- **Singular tests** are `tests/*.sql`: templated queries that pass when they
  return no rows. A singular test belongs to every model it `ref()`s, and
  must ref at least one (tests on sources only are out of v1).
  `{{ config(severity='warn') }}` is the only config a test takes.
- `severity`: `error` (default) or `warn`.
- **Tests run before the commit.** A model's tests run inside its
  transaction, after it is built and before `COMMIT`. An `error` failure
  rolls the model back, so its previous version stays and the bad data is
  never published. The model is marked `error` and everything downstream of
  it is `skipped`. A `warn` failure is reported, and the model commits.
- **A singular test that refs several models** can't run inside one
  transaction, because each model has its own. It runs once all of those
  models have committed. An `error` failure can't roll them back: it marks
  the test `error` and skips the models downstream of all of them (those
  wait for the test). A model the test also checks doesn't wait for it.
- `test [select]` runs only the tests, without building anything.

## Selection

`-select` (and job `select`) takes one or more space-separated terms, joined
as a union. `-exclude` takes the same syntax and removes models. Up- and
downstream follow refs and relationships tests.

| Term                  | Selects                                    |
|-----------------------|--------------------------------------------|
| `orders`              | The model                                  |
| `+orders`             | The model and everything upstream          |
| `orders+`             | The model and everything downstream        |
| `+orders+`            | Both                                       |
| `source:raw.orders+`  | Everything downstream of a source table    |
| `source:raw+`         | Everything downstream of any of its tables |
| `tag:daily`           | Models with the tag (`+` works here too)   |
| `path:models/staging` | Models under that folder (`+` works here too) |

With no selection, every model is selected. A term that matches nothing is
an error, and so is a selection that leaves no models, so a typo can't
silently select nothing. A source term needs its trailing `+`.

## Running

- Selected models run in DAG order. Up to `threads` models run at once,
  each in its own `duckdb` process: attach the lake, `USE lake`, `BEGIN`,
  build, run its tests, `COMMIT`, then write its `run_history` rows.
  Each commit's message is `run <run_id>: <model>`
  (`FROM ducklake_snapshots('lake')`).
- A model whose commit conflicts with another writer's (DuckDB's
  `TransactionContext` error) is built again, up to 3 times. With a SQLite
  catalog, concurrent writers lock each other out: use `threads: 1`.
- SQL runs in the project's time zone (`timezone`, default UTC).
- A model that fails, either building or on an `error` test, rolls back (its
  previous version stays), and `skipped` is recorded for everything
  downstream of it. Independent branches keep going.
- Model statuses: `success`, `warn` (built, a `warn` test failed), `error`,
  `skipped`.
- Exit codes: `0` everything `success` or `warn`; `1` anything `error` or
  `skipped`; `2` the project or the config could not be loaded or compiled.
- On SIGINT/SIGTERM, running models are cancelled (rolled back), and models
  not yet started are `skipped`.

## Targets

| Target | Who writes it                    | `ref()` resolves to         | `source()` resolves to |
|--------|----------------------------------|-----------------------------|------------------------|
| `prod` | only `lakeshape serve`           | `<schema>.<model>`          | prod                   |
| `dev`  | the CLI (`run`, `test`), always  | `dev_<user>__<schema>.<model>` | prod                |

- `<user>` is `$USER`, lowercased, with anything other than `a-z 0-9 _`
  turned into `_`. `-dev-user` overrides it.
- **ref() is always dev in dev.** A model whose upstream was never built in
  dev fails up front, before anything runs, e.g. ``ref('stg_orders') →
  dev_nicolas__staging.stg_orders does not exist; run `+my_model` ``. In
  prod, such a model fails (and its downstream is skipped) while the rest of
  the run goes on.
- **In dev, lakeshape only writes schemas that start with `dev_<user>__`.**
  Any other write fails before it is sent to DuckDB. This is a guard in
  lakeshape, not a permission: dev credentials can write to the lake bucket
  and catalog. Separate credentials would be needed to make it a hard limit.
- `compile` and `ls` can show prod with `-target prod`; they only read files.
- The CLI has no way to write prod. Prod changes go through the service: deploy
  the project, and a job (or `lakeshape trigger <job> -full-refresh`, see
  below) builds it.
- CLI runs publish nothing to relay; only the service does.

## Service (`lakeshape serve`)

Runs jobs on the prod target. On the server, it runs from systemd with the
project deployed to `/etc/lakeshape/project`.

```yaml
# jobs.yml
jobs:
  - name: orders
    select: source:raw.orders+
    on:
      - topic: duckpipe.ingest
        type: ok                          # comma list allowed
        data: {table: lake.raw.orders}    # every key must be equal
    debounce: 60s                         # default 60s
    max_wait: 10m                         # default 10m
  - name: nightly
    select: tag:daily
    schedule: "0 3 * * *"                 # 5-field cron, project timezone
```

- A job has a `name`, a `select`, and at least one of `schedule` and `on`.
  `-exclude`, `-full-refresh` and `-vars` can be set per job as
  `exclude`, `full_refresh` and `vars`. Jobs are checked whenever the
  project loads, so `lakeshape ls` catches a broken `jobs.yml` on your Mac.
- **`on` filters.** `topic` uses relay's filter syntax (`duckpipe.*`).
  `type` is one type or a comma list. `data` matches top-level fields of the
  event's `data` by equality. Every given field must match. lakeshape's own
  `lakeshape.*` events never trigger jobs, so `*` and `lakeshape.*` are
  refused as topics.
- **Debounce.** A matching event starts or resets the job's timer. The job
  becomes pending when no matching event has arrived for `debounce`, or
  `max_wait` after the first one, whichever comes first. A 1,000-file
  duckpipe backfill (about 20 events) is therefore one run.
- **One run at a time.** A single executor runs everything. When it is free,
  it takes every pending job, joins their selections into one run, and
  records which jobs it includes. A job that becomes pending while a run is
  going is run once afterwards, however many times it was triggered. Jobs
  with different `vars` or `full_refresh` can't share a run: each such
  group runs on its own, one after another.
- **Events are acknowledged after the run.** lakeshape reads relay through a
  named consumer, `lakeshape`. The cursor moves past an event only once the
  run that event triggered has finished, whether it passed or failed. After
  a crash or restart, unacknowledged events are replayed and trigger the job
  again. Rebuilding is idempotent, so duplicates are harmless.
- **Missed schedules.** At startup, a scheduled job whose last fire time
  since its last run (from `run_history`) was missed runs once. Missed slots
  are not backfilled one by one. A scheduled job that never ran waits for
  its next fire time.
- **Reload.** SIGHUP reloads the project and the jobs between runs. A project
  that doesn't load or compile is refused: the old one keeps running and
  `lakeshape.project failed` is published.
- **Without relay** (nothing configured), `on` jobs never fire, a warning is
  logged at startup, and schedules still work.
- `lakeshape trigger <job> [-full-refresh]` (run on the server, as `sudo
  lakeshapectl trigger <job>`) asks the running service to run a job now, by
  publishing `lakeshape.trigger requested` to relay with a new `run_id`. The
  service runs it on its own, with that run ID, after the current run. The
  command follows the run's events and exits 0 or 1 with it; Ctrl-C stops
  waiting, not the run. A job always runs in prod: flags that only apply to
  dev or to other commands (`-dev-user`, `-target`, `-select`, ...) are
  refused, as on every command.
- A new binary restarts the service: a run in progress rolls back. A new
  project only reloads it, between runs.

## relay events

| Topic               | Type                         | When / data                                                |
|---------------------|------------------------------|------------------------------------------------------------|
| `lakeshape.run`     | `started`, `ok`, `failed`    | Per run. `jobs`, `models`, `duration_ms`, and `error` (the models that failed) |
| `lakeshape.model`   | `ok`, `warn`, `failed`, `skipped` | Per model. `model`, `relation`, `materialized`, `rows`, `duration_ms`, `error`, failed `tests` |
| `lakeshape.project` | `failed`, `ok`               | A reload was refused, or loaded again after a refusal       |

Every event of one run shares its `run_id`. vigil alerts on `failed`. For an
alert when a job hasn't run, deploy vigil with `EXPECT="lakeshape/run=…"`.

## Run history

`lake.lakeshape.run_history` in prod. In dev it is
`lake.dev_<user>__lakeshape.run_history`. There is one row per model and
per test. A model's rows are written by its own `duckdb` process right
after its commit, in a transaction of their own, so they can carry the
snapshot id. Rows of models that failed (rolled back) or were skipped, and
of tests on several models, are written together at the end of the run.

| Column                        | Meaning                                        |
|-------------------------------|------------------------------------------------|
| `run_id`                      | The run                                        |
| `jobs`                        | Jobs the run included (empty for CLI runs)     |
| `target`                      | `prod` or `dev`                                |
| `kind`                        | `model` or `test`                              |
| `name`                        | Model name, or test id (`unique_orders_order_id`) |
| `relation`                    | `schema.table` built, or those a test checks (comma-separated) |
| `materialized`                | For models                                     |
| `status`                      | `success`, `warn`, `error`, `skipped`          |
| `rows`                        | Rows written (models) or failing rows (tests)  |
| `error`                       | Why it failed                                  |
| `started_at`, `finished_at`   | When                                           |
| `snapshot_id`                 | DuckLake snapshot the model committed          |

## Commands

```sh
lakeshape compile [-select …]      # the rendered SQL of each model, to stdout
lakeshape ls [-select …]           # models in DAG order, with schema, materialization, tags; -orphans
lakeshape run [-select …] [-exclude …] [-full-refresh] [-vars …]   # dev target
lakeshape test [-select …]         # dev target
lakeshape history [model]          # from run_history, newest first (a model's tests too)
lakeshape serve                    # the service (prod)
lakeshape trigger <job> [-full-refresh]  # on the server: ask the service to run a job now
```

Flags shared with the other tools: `-project` (default `.`), `-duckdb`,
`-endpoint-url`, `-region`, `-profile`, `-state-dir`. Lake credentials come
from the usual AWS and `PG*` environment variables. On the server they come
from `/etc/lakeshape.env`.

## Deploy

- `deploy.sh` (run on your Mac) checks that the project loads, builds a
  static Linux binary, copies the binary, `install.sh` and the project
  (without dot files) over SSH, and runs `install.sh` with sudo.
- `install.sh` (run on the server, after `ducklake-setup.sh`) installs the
  DuckDB CLI if needed, `lakeshape` and `lakeshapectl`, creates the
  `lakeshape` user, a Garage key that can read and write the lake bucket,
  and `/etc/lakeshape.env` (that key, plus the catalog's `PG*` settings from
  `/etc/ducklake.env`). It swaps the project into
  `/etc/lakeshape/project`, putting the old one back if the new one
  doesn't load there, and writes the systemd unit with the same hardening
  as the other services. It restarts the service when the binary or unit
  changed, and reloads it when only the project did.
- `deploy.sh` takes `HOST`, `SSH_PORT` and `PROJECT` (the local project
  directory, `../analytics` next to the script by default), and passes
  `LAKE_BUCKET`, `KEY_NAME`, `ENDPOINT` and `DUCKDB_VERSION` on to
  `install.sh`. `undeploy.sh` removes it (`PURGE=1`
  also removes the deployed project and the user; the lake is never
  touched).

## Out of v1

Ephemeral models, seeds, snapshots (SCD2), packages, hooks, a docs site,
source freshness, tests on sources, a whole-run atomic commit, concurrent
runs, and writing prod from the CLI.
