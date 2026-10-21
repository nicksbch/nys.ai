# lake-ui

A small web UI to query the DuckLake set up by `../linux-scripts/ducklake-setup.sh`
(the tables duckpipe loads). It is a single Go binary with the page built in.

- **Query:** a SQL editor; Run (⌘↵ / Ctrl↵) runs the whole editor, or just the
  selected text. Cancel (Esc) stops the query on the server too.
- **Autocomplete:** while typing, the editor suggests SQL keywords, schemas,
  tables (`raw.` lists raw's tables) and the columns of the tables the query
  names (`o.` after `FROM raw.orders o` too). ↑/↓ to choose, Enter or Tab to
  insert, Esc to close, Ctrl+Space to open it by hand.
- **Results:** a table with column types, NULLs marked, numbers right-aligned.
  The first 10,000 rows are shown.
- **Download CSV:** every row of the query, streamed straight from DuckDB.
- **Tables:** the lake's schemas, tables and columns in a sidebar, with a
  filter. "query" on a table runs `SELECT * FROM <table> LIMIT 100`.

The lake is attached **read-only** as `lake` and is the default database, so
`SELECT * FROM raw.orders` works. One statement runs at a time, and it must
return rows: `SELECT`, `FROM`, `DESCRIBE`, `SHOW`, `SUMMARIZE`, DuckLake's
functions such as `FROM ducklake_snapshots('lake')`, and time travel
(`FROM raw.orders AT (VERSION => 3)`). Anything else (`CREATE`, `INSERT`,
`SET`, `ATTACH`, several statements) is refused.

## How it works

Each request runs the official `duckdb` CLI (1.5 or newer, installed by
`ducklake-setup.sh`) in a fresh in-memory process, like duckpipe does, so
lake-ui needs no cgo. The script attaches the lake read-only, then runs the
SQL from the page inside DuckDB's `query()` function. `query()` accepts one
statement that returns rows. Because the SQL is passed as a string literal,
the CLI can't read any of it as a dot command (`.shell`, `.output`, ...).
Up to 4 queries run at once; more wait their turn. A query is stopped after
5 minutes.

The service has its own **read-only credentials**, made by `install.sh`.
Whatever SQL is typed runs with them:

| What        | Credential                                                             |
|-------------|------------------------------------------------------------------------|
| Catalog     | PostgreSQL role `lake_ui`: SELECT on the catalog tables, including the ones DuckLake adds later |
| Data        | Garage key `lake-ui-key`: read-only on the `lake` bucket               |
| Server      | systemd `DynamicUser`, read-only file system except its state directory |

## Files

| File          | What it is                                                                     |
|---------------|--------------------------------------------------------------------------------|
| `main.go`     | The server: JSON API for the page, results, CSV download                       |
| `duck.go`     | Runs scripts with the `duckdb` CLI; attaches the lake; wraps the SQL in `query()` |
| `index.html`  | The page (plain HTML and JavaScript, built into the binary)                    |
| `deploy.sh`   | Run on your Mac: builds the binary and installs it on the server               |
| `install.sh`  | Run on the server by `deploy.sh`: installs the binary, the read-only key and role, and the service |

## Deploy

```sh
./deploy.sh                      # or: ONLY=lake-ui ../deploy-all.sh
```

Run `ducklake-setup.sh` first (`deploy-all.sh` does it in the right order).
The first install prints the login; it is also in `/etc/lake-ui.env`
(`sudo grep LAKE_UI_AUTH /etc/lake-ui.env`). The UI listens on
`http://<server>:8092`.

| install.sh setting | Default                  | Meaning                                      |
|--------------------|--------------------------|----------------------------------------------|
| `LISTEN`           | `[::]:8092`              | Address to serve on; `127.0.0.1:8092` for local only |
| `LAKE_BUCKET`      | `lake`                   | Bucket the key may read                      |
| `KEY_NAME`         | `lake-ui-key`            | Garage key for the service                   |
| `PG_ROLE`          | `lake_ui`                | PostgreSQL role for the service              |
| `UI_USER`          | `admin`                  | Basic-auth user (first install only)         |
| `ENDPOINT`         | `http://127.0.0.1:3900`  | S3 endpoint (first install only)             |

## Run it locally

```sh
go build -o lake-ui . && ./lake-ui     # then open http://127.0.0.1:8092
```

It needs the `duckdb` CLI and credentials in the environment. To use the
server's lake from your Mac, reuse the values from `/etc/lake-ui.env`. The
`lake_ui` role can only connect from the server itself, so use the values from
`/etc/ducklake.env` instead, with `PGHOST=192.168.1.169`:

```sh
PGHOST=192.168.1.169 PGUSER=ducklake PGPASSWORD=...
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
AWS_ENDPOINT_URL=http://192.168.1.169:3900   # this is the default
```

Note that this gives lake-ui write access to the lake. The attach is still
read-only and `query()` still refuses writes.

| Flag            | Default                                    | Meaning                                    |
|-----------------|--------------------------------------------|--------------------------------------------|
| `-listen`       | `127.0.0.1:8092`                           | Address to serve the UI on                 |
| `-auth`         | `$LAKE_UI_AUTH`                            | Require a login, given as `user:pass`      |
| `-catalog`      | `$LAKE_UI_CATALOG`, else `ducklake:postgres:dbname=ducklake` | DuckLake catalog to attach |
| `-endpoint-url` | `$AWS_ENDPOINT_URL`, else `http://192.168.1.169:3900` | S3 endpoint                     |
| `-duckdb`       | `duckdb`                                   | DuckDB CLI to run queries with             |
| `-state-dir`    | DuckDB's own                               | Where DuckDB keeps extensions and temp files |
| `-max-rows`     | `10000`                                    | Rows shown per query (CSV has no limit)    |
| `-timeout`      | `5m`                                       | Longest a query may run                    |

## Logs

```sh
journalctl -u lake-ui -f
```

Failed queries caused by the SQL itself (typos, unknown columns) are shown in
the page, not logged.
