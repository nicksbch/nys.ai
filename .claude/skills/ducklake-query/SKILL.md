---
name: ducklake-query
description: Run SQL against the DuckLake on the server, read-only, to answer questions about the data, check what was loaded, or look at tables, columns and values. Use whenever a question needs the lake's data ("how many…", "what's in…", "show me…", "did … load"), or before writing a model or pipe that depends on what is already there.
---

# Querying the DuckLake

The lake is queried on the server with the `ducklake` command, over ssh,
without sudo. It opens the DuckDB CLI with the lake attached as `lake`
(and `USE lake`), **read-only**: any write fails with "attached in read-only
mode". Changing the lake is the pipeline's job (duckpipe, lakeshape), never
a query's.

```sh
ssh -p 2222 192.168.1.169 'ducklake -csv -c "SELECT count(*) FROM uk.onspd"'
```

- `-c` once per statement; several `-c` run in order. Put output flags
  (`-csv`, `-json`, `-line`) before the first `-c`: `-csv` or `-json` are
  the easiest to read back, the default box format truncates wide rows.
- Quote with care: the SQL sits inside `'…'` for ssh and `"…"` for the
  shell on the server. A SQL string literal is `'"'"'text'"'"'`; for long
  queries send the SQL on stdin instead:
  `ssh -p 2222 192.168.1.169 ducklake -csv < query.sql`.
- Each call takes a few seconds to attach. Batch related statements into one
  call rather than one ssh per query.
- Always `LIMIT` exploratory selects; the raw tables have tens of millions of
  rows.
- `ducklake: run with sudo, or join group lake-readers…` means the ssh user
  isn't in `READ_USERS` (deploy.env, `ONLY=ducklake ./deploy-all.sh`).
  `sudo ducklake` is read-write and needs a password: don't use it to get
  around this.

## What's in the lake

| Schema      | What                                                          | Query it for                    |
|-------------|---------------------------------------------------------------|---------------------------------|
| `<area>` (e.g. `uk`) | raw tables from duckpipe: `record` (JSON), `_file`, `_loaded_at` | what arrived, per file         |
| `staging`   | `stg_<table>`: typed, cleaned, one release rule applied       | **answers**: start here         |
| `marts`     | aggregates built for questions (e.g. `price_paid_monthly`)   | answers the semantic layer serves |
| `duckpipe`  | `load_history`: one row per file loaded                       | did a file load, when, rows     |
| `lakeshape` | `run_history`: model builds and test runs                      | did a job / test pass           |

Answer from `staging` or `marts`, not the raw tables: the raw tables keep
every release and every changed record, so counts on them are wrong for most
questions. What a column means, the grain and the release rule are in
`analytics/models/staging/stg_<table>.sql` (header comment) and `.yml`
(descriptions, tests); read them before interpreting a result. Metrics already
defined for agents are in `semantic/models.yml`.

Find things:

```sql
SELECT schema_name, table_name, estimated_size FROM duckdb_tables() WHERE database_name = 'lake' ORDER BY ALL;
SELECT schema_name, view_name FROM duckdb_views() WHERE database_name = 'lake' ORDER BY ALL;
DESCRIBE staging.stg_onspd;
SELECT * FROM ducklake_snapshots('lake') ORDER BY snapshot_id DESC LIMIT 5;  -- recent changes
```

A raw field: `record->>'pcds'` (text) or `record->'pcds'` (JSON).

## Reporting

Give the number and the query that produced it, and say which table and
which release rule it rests on (e.g. "latest ONSPD file only"). If the data
looks wrong (duplicates on a grain key, a missing month, sentinels that
weren't nulled), say so; fixing it is a change to the pipe or staging model
(the `ducklake-ingest` skill), not something to work around in the query.
