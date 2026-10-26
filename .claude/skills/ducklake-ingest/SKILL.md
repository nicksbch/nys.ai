---
name: ducklake-ingest
description: The standard way to bring a new dataset from Garage S3 into the DuckLake (duckpipe pipe, lakeshape source, staging model, tests, job). Use whenever a new file, folder or dataset lands in a bucket and must be "integrated", "loaded", "ingested" or "added to the pipeline", or when an existing pipe or staging model is changed.
---

# Integrating a dataset into DuckLake

The path every dataset takes:

```
s3://home/<area>/<dataset>/…      raw files, exactly as downloaded (url-to-bucket, or uploaded)
  └─ preprocessor (zip only)      unzips to <prefix>/extracted/
      └─ duckpipe pipe            lake.<area>.<table>: record JSON, _file, _loaded_at
          └─ lakeshape source     analytics/sources.yml
              └─ staging view     analytics/models/staging/stg_<table>.sql + .yml (tests)
                  └─ job          analytics/jobs.yml: tests run after each load
```

Marts and the semantic layer (`semantic/models.yml`) are **not** part of an
integration. They are built when a question needs them; propose them, don't
add them unasked (CLAUDE.md: no scope creep).

Work through the steps in order. Each one has a check; don't move on until it
passes.

## 1. Look at the files, don't assume

```sh
aws s3 ls --recursive --human-readable s3://home/<prefix>/
aws s3api get-object --bucket home --key <key> --range bytes=0-4000 /dev/stdout | head -5
```

Note: format (csv / json / parquet / zip), header or not, delimiter, quoting,
size, and how files are named (one file per release? per month? per region?).
A `.zip` needs a preprocessor job first (step 3); duckpipe can't read it.

If the key has a typo or an odd layout, keep the pipe pointed at where the
data really is and tell the user. Never move or rename files in the bucket
without asking.

## 2. Profile the data with DuckDB, using duckpipe's read options

Load it into a scratch DuckDB in the scratchpad, with **the same options the
pipe will use**, so what you see is what the staging model will see:

```sh
duckdb <scratchpad>/<dataset>.duckdb -c "
CREATE SECRET (TYPE s3, PROVIDER credential_chain, REGION 'garage',
               ENDPOINT '192.168.1.169:3900', URL_STYLE 'path', USE_SSL false);
CREATE TABLE t AS FROM read_csv('s3://home/<key>', header = true, all_varchar = true, normalize_names = true);"
```

The local DuckDB may be older than 1.5: fine for reading S3 files, but it
can't open the lake itself. Find out, and write down for the model's header
comment and yml:

- **Grain:** what one row is, and the key that is unique (`count(distinct)`
  vs `count(*)`).
- **Release semantics:** how a newer file relates to an older one (step 6).
- **Sentinels:** values that mean "none": empty strings, `99999999`-style
  pseudo codes, `99.999999` coordinates, `N/A`. Count them. Look at the most
  common value of every code column too: "none" codes don't always follow a
  pattern (ONSPD's `E65000001` is "not in a national park").
- **Formats:** dates (`YYYYMM`? `YYYY-MM-DD HH:MM`?), numbers, flags.
- **Code columns:** the distinct values of low-cardinality columns
  (`accepted_values` candidates).
- **Join keys** to datasets already in the lake (postcode, LSOA code,
  force…), and whether their formats match.

## 3. Unzip (zip only): `preprocessor/preprocessor.yaml`

```yaml
  - name: <area>_<dataset>
    bucket: home
    prefix: <area>/<dataset>/
    pattern: "*.zip"
    to: <area>/<dataset>/extracted/
```

## 4. The pipe: `duckpipe/duckpipe.yaml`

```yaml
  # <Publisher> <dataset> (<file name pattern>, <how often>): <layout in one
  # line>. <header / no header>. Kept as text; cast later.
  - name: <table>
    bucket: home
    prefix: <area>/<dataset>/                 # extracted/ for zips
    pattern: "<as narrow as the file names allow>"
    table: <area>.<table>
    options: "header = true, all_varchar = true, normalize_names = true"
```

Rules:

- **Table:** `<area>.<table>` (e.g. `uk.price_paid`). Schema = the area
  (country or domain), table = a short readable name of the dataset, not the
  bucket folder's spelling. One table per file kind with its own columns.
- **Name:** the table without its schema. Never rename a pipe later: it
  would load everything again.
- **Pattern:** match only the data files (`ONSPD_*_UK.csv`, `*-street.csv`),
  not readmes, lookups or docs that may arrive in the same folder.
- **CSV options:** always `all_varchar = true` (the same field has the same
  JSON type in every file; casting happens in staging). With a header,
  `normalize_names = true`. Without one, name the columns:
  `header = false, names = [...]`.
- **Comment** above every pipe: publisher, file naming, cadence, layout.
- The bucket must be in duckpipe's `SOURCE_BUCKETS` (default `home`).

Check: `cd duckpipe && go test ./... && go run . -config duckpipe.yaml check`.

## 5. The source: `analytics/sources.yml`

Add the table to its area's `tables:` list.

## 6. The staging model: `analytics/models/staging/stg_<table>.sql`

A view (the staging folder's default; never set `config()` here) with:

1. **A header comment:** what one row is, and which rows are kept.
2. **A `raw` CTE** that picks every field out of `record` with `->>`, keeps
   `_file` and `_loaded_at`, and applies the release rule below.
3. **A final select** that casts types, turns sentinels and empty strings
   into `null`, and turns `Y`/`N` or `0`/`1` flags into booleans with an
   `is_`/`has_` name. Keep `_file` and `_loaded_at` at the end.

Column names stay the source's (after `normalize_names`), so anyone can match
them to the publisher's docs; readable business names belong in marts and the
semantic layer. Explain each column in the yml (step 7) instead.

Pick the release rule from step 2. These are the ones in use:

| The files are…                                  | Keep                                  | Example            |
|-------------------------------------------------|---------------------------------------|--------------------|
| Changes (add / change / delete) to one dataset  | latest row per business key, drop deletes | `stg_price_paid`   |
| Partitions that can be re-extracted / re-sent   | latest load of each file              | `stg_crime_street` |
| Full snapshots; each release replaces the last  | rows of the latest file only          | `stg_onspd`        |

```sql
-- latest row per key
qualify row_number() over (partition by <key> order by _loaded_at desc, _file desc) = 1
-- latest load of each file
qualify _loaded_at = max(_loaded_at) over (partition by _file)
-- latest file only
qualify _file = arg_max(_file, _loaded_at) over ()
```

A logic repeated across many columns (e.g. a sentinel rule) goes in a macro
in `analytics/macros/<name>.sql`, with a comment saying what it is for.

## 7. Tests: `analytics/models/staging/stg_<table>.yml`

- `description` on the model and on each column whose name isn't obvious
  (code columns: what the code is, and which geography or year).
- The grain key: `[not_null, unique]` (error: a failure rolls the view back).
- Columns that must always be there: `not_null`.
- Code columns with a known set: `accepted_values` with `severity: warn`
  (publishers add values; a new one should alert, not block).
- Joins to other staging models: `relationships` with `severity: warn`.

## 8. The job: `analytics/jobs.yml`

```yaml
  # Test stg_<table> after duckpipe loads new <dataset> files (the view reads
  # new rows by itself).
  - name: <table>
    select: source:<area>.<table>+
    on:
      - topic: duckpipe.ingest
        type: ok
        data: {table: lake.<area>.<table>}
```

Many files per delivery (hundreds of CSVs from one archive): add
`debounce: 5m` and `max_wait: 1h`, as the `crime` job does.

## 9. Check before deploying

```sh
cd duckpipe && go test ./... && go run . -config duckpipe.yaml check
cd lakeshape && go run . -project ../analytics -target prod ls                         # project loads
cd lakeshape && go run . -project ../analytics -target prod compile -select stg_<table>  # the SQL
```

Then run the compiled SQL against the scratch DB from step 2, with the
source table replaced by a copy shaped as duckpipe writes it:

```sql
CREATE TABLE src AS
SELECT to_json(t) AS record, 's3://home/<key>' AS _file, now() AS _loaded_at FROM t;
```

and run each yml test by hand (count nulls, duplicates of the key, values
outside `accepted_values`). Every `error` test must pass on the real file.

## 10. Deploy (ask first) and confirm the load

Deploying changes the server: ask the user before running it.

```sh
ONLY="duckpipe lakeshape" ./deploy-all.sh        # add preprocessor for zips
```

Then confirm through the agent interfaces (mcp-hub `duckpipe__load_history`,
`lakeshape__…`), or on the server: `sudo duckpipectl history <pipe>`, and
that the job ran with its tests passing. A large file can take minutes.
Query the lake read-only (no sudo) to check the rows:
`ssh -p 2222 192.168.1.169 'ducklake -c "SELECT count(*) FROM <area>.<table>"'`.

## 11. Finish

- Commit as `(duckpipe, analytics) ingest <dataset>`; one commit per
  integration.
- Follow-ups you noticed (a mart, a semantic model, a vigil `EXPECT` for the
  dataset's cadence, a misnamed folder) go to the user as suggestions, or to
  `TODO.md` as `idea`, never built in the same change.
