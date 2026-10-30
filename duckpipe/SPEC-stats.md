# duckpipe row counts: spec (approved 2026-10-28, built; slimmed 2026-10-29)

Every file duckpipe loads has its records counted by Go before it is
loaded, without DuckDB, and the count is saved on its `load_history` row.
After the insert, the file's rows in the table must be that count, or the
file fails, like any bad file. Later, `verify` checks that the tables still
hold what was loaded. The goal: one record in a file is one row in its
table, shown without someone checking it by hand.

"The table" is the pipe's target table (e.g. `uk.price_paid`). lakeshape's
models are out of scope (see the end).

## Why the current `rows` isn't enough

`load_history.rows` is counted from the target table after the insert. It
shows what DuckDB's parser made of the file, not what the file held. A
parse that merges, splits or skips rows would still load.

## What is measured

| Stat | How | Why |
|------|-----|-----|
| `bytes` | the S3 object's size | already known |
| `records`, `fields` | Go's `encoding/csv`, streamed from S3 (gunzipped if `.gz`), with the pipe's `delim` and `header`; every record must have as many fields as the first | **doesn't use DuckDB's parser or sniffer**. A quoted newline stays inside one record. An unquoted one breaks the field count, and the file fails, as bad data |
| `columns` | DuckDB's `DESCRIBE` of the file with the pipe's options (only sniffs it) | the columns DuckDB will read |

For NDJSON, `records` is the number of non-empty lines. For Parquet, it is
`num_rows` from `parquet_file_metadata`, which doesn't need a parse.
Neither has `fields` or `columns`.

Go can't guess like DuckDB's sniffer, so a CSV pipe sets `header`, and
`delim` if it isn't a comma. `duckpipe check` rejects a CSV pipe without
`header`.

## Checks

| Check | Fails when | When |
|-------|------------|------|
| `encoding/csv` reads the whole file | bad quoting, or rows with the wrong number of fields | before the load; nothing is inserted |
| `fields` = `columns` | Go and DuckDB disagree on the columns (a quoted comma splitting a row, the wrong `delim`) | before the load |
| `records` = the file's rows in the table | rows were lost, merged or split on the way in | inside the load's transaction: the file's history row is written as `failed`, and its rows are deleted from the table again before the commit. The other files of the batch stay |

A failed check works like any failed file: status `failed`, the reason in
`error` (e.g. `check: rows: 1204 in the file (encoding/csv), in the table
1203`), on the page's Failed card, in `load_history`, as `duckpipe.ingest`
`failed` on relay, so vigil shows it. Then `retry`, once the file or the
pipe's options are fixed.

## Storage

One column added to `load_history` (`ADD COLUMN IF NOT EXISTS`, as was
done for `target`), written in the same insert as the row:

| Column | Example |
|--------|---------|
| `file_stats JSON` | `{"bytes": 98213, "records": 1204, "fields": 12, "columns": 12}` |

Rows from before this have `NULL`.

## Re-checking later: `verify`

A table can lose rows after a load: on 2026-10-23 the crime tables were
dropped and their history kept saying `loaded`. `duckpipe verify [pipe]`
compares, per file, the rows of its `loaded` history rows with its rows in
the table now (in both directions: rows no history row loaded show too).
Because it uses `rows`, it covers files loaded before the count too. It
runs after each maintenance, every 6h, and from the CLI. A file that no
longer matches is:

- published to relay as `duckpipe.verify` `failed`, with the file and the
  difference, so vigil shows it;
- listed on the page in a **Drifted** card (as the Failed card is), by the
  `verify` CLI, and in `list_pipes`.

The history isn't changed. The fix is a reload (TODO, "Reload a table from
an agent"). The drifted list is kept in memory, from the last verify, and
a restart runs verify again.

## Interfaces

- **CLI:** `duckpipe history` gets a `check` column (`ok`, `failed` with
  the reason, `skipped`, or `-` for rows from before this).
  `duckpipe verify [pipe]`.
- **MCP:** `load_history` rows carry the `check`. `list_pipes` also returns
  the drifted files (the first 50, and how many).
- **Page:** the history's Check column; the Drifted card.

## Cost

Each file is read once more, streamed from S3 by `encoding/csv`. The
`DESCRIBE` only sniffs the start of the file, and the table's count is on
the rows just inserted. Garage is on the same machine. Per pipe,
`stats: false` turns it all off.

## Dropped (2026-10-29)

The first build also took column stats (non-null, distinct, min, max) from
the file and from the table, with a float tolerance, a `lake_stats` column,
a `file_stats` MCP tool and a stats view on the page. Dropped: the table's
rows are `to_json` of the same `read_csv`, so comparing them checked DuckDB
against itself, and it doubled load times. Column profiles, if wanted,
belong in lakeshape tests, not the load path.

## Not in this spec

- Counts through lakeshape's models. Staging dedupes and drops rows by
  design, so a file's count isn't expected to survive into `stg_*`.
- Backfilling files loaded before this.
- `.zst` files: counting them needs a new dependency, and no pipe uses
  `.zst` today. A `.zst` file loads uncounted, with `file_stats` set to
  `{"skipped": "zst"}`.
- Options that change which lines are rows (`skip`, `comment`, `new_line`,
  …), quotes other than `"`, and JSON options. `check` rejects a pipe
  using them unless it sets `stats: false`. No pipe uses them today.

## Code

- `stats.go`: the options Go reads, the `encoding/csv` / NDJSON / Parquet
  counts, the `DESCRIBE`, and the table-side count SQL.
- `verify.go`: `verify`, the drifted files, their relay events.
- `engine.go`: the column; `loadBatch` counts first and fails the files
  that don't pass; `load` checks the table's rows and deletes the failed
  files' rows inside its transaction; `historyRows` returns the check.
- `main.go` (`verify`), `ui.go` / `ui.html` (check column, Drifted card),
  `config.go` (`stats: false`; rejecting unsupported options), `README.md`.

## Tests

- A clean CSV with a quoted comma and a quoted newline: loads, `ok`.
- An unquoted newline inside a row: fails before the load.
- A `;` file in a pipe without `delim`: fails on the columns.
- A file whose table rows differ from its records (forced): fails, its rows
  deleted, the batch's other file stays.
- Rows deleted from the table after loading, and rows no history loaded:
  `verify` reports them as drifted.
