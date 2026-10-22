# semantic

The [Boring Semantic Layer](https://github.com/boringdata/boring-semantic-layer)
(BSL) over the lake's marts, served as an MCP server over HTTP. Agents ask
for measures by dimensions (`sales` and `avg_price` by `district` per
`month`) instead of writing SQL. It is the only Python project in this repo.

- **Models:** `models.yml`, in BSL's YAML format. Each model names a table in
  the lake's `marts` schema (the models lakeshape builds from
  `../analytics/models/marts`) and gives it dimensions and measures.
- **MCP:** streamable HTTP at `http://<server>:8097/mcp`, with BSL's tools:
  `list_models`, `get_model`, `get_time_range`, `query_model`,
  `compare_periods`, `search_dimension_values`. Clients send
  `Authorization: Bearer <token>`.
- **Health:** `GET /healthz` (no token).
- **mcp-hub:** agents reach it through the hub as `semantic__*` tools;
  mcp-hub's install copies the token from `/etc/semantic.env`.

## How it works

`server.py` opens an in-memory DuckDB (in-process, through ibis), attaches the
lake **read-only** as `lake`, loads `models.yml` over every table in
`lake.marts` and serves the models with BSL's `MCPSemanticModel` (FastMCP).
Tool calls run one at a time, since they share one DuckDB connection.

The service has its own **read-only credentials**, made by `install.sh`, as
lake-ui does:

| What        | Credential                                                             |
|-------------|------------------------------------------------------------------------|
| Catalog     | PostgreSQL role `semantic`: SELECT on the catalog tables               |
| Data        | Garage key `semantic-key`: read-only on the `lake` bucket              |
| MCP         | `SEMANTIC_MCP_TOKEN` in `/etc/semantic.env`                            |
| Server      | systemd `DynamicUser`, read-only file system except its state directory |

## Adding a model

Add the mart to `../analytics`, then a model for it to `models.yml`:
dimensions are column expressions (`_.district`), measures aggregate
(`_.sales.sum()`), calculated measures combine measures
(`_.total_value / _.sales`). Mart rows are already aggregated, so a measure
must re-aggregate correctly: sums of counts, averages weighted by counts, and
no medians. Add a test to `test_models.py` with the mart's columns.

## Files

| File             | What it is                                                               |
|------------------|--------------------------------------------------------------------------|
| `models.yml`     | The semantic models                                                      |
| `server.py`      | Attaches the lake, loads the models, serves MCP; `check` loads and exits |
| `test_models.py` | The models over in-memory tables, and the token check                    |
| `pyproject.toml`, `uv.lock` | Dependencies, pinned (DuckDB matches the server's CLI)        |
| `deploy.sh`      | Run on your Mac: tests, then installs on the server                      |
| `install.sh`     | Run on the server by `deploy.sh`: uv, Python, the read-only key and role, the service |

## Deploy

```sh
./deploy.sh                      # or: ONLY=semantic ../deploy-all.sh
```

Run `ducklake-setup.sh` and lakeshape first (`deploy-all.sh` does it in the
right order). Before the new app replaces the old one, `install.sh` runs
`server.py check` with the service's credentials: models that don't load
against the lake stop the deploy; a lake that can't be reached only warns.
The token is in `/etc/semantic.env`
(`sudo grep SEMANTIC_MCP_TOKEN /etc/semantic.env`); deploy mcp-hub after the
first install so the hub picks it up (`deploy-all.sh` runs it after semantic).

| install.sh setting | Default                  | Meaning                                      |
|--------------------|--------------------------|----------------------------------------------|
| `LISTEN`           | `[::]:8097`              | Address to serve on; `127.0.0.1:8097` for local only |
| `LAKE_BUCKET`      | `lake`                   | Bucket the key may read                      |
| `KEY_NAME`         | `semantic-key`           | Garage key for the service                   |
| `PG_ROLE`          | `semantic`               | PostgreSQL role for the service              |
| `ENDPOINT`         | `http://127.0.0.1:3900`  | S3 endpoint (first install only)             |

## Run it locally

```sh
uv run pytest                    # tests, no lake needed
uv run python server.py serve    # then http://127.0.0.1:8097/mcp
```

`serve` and `check` need credentials in the environment, as for lake-ui: the
values from `/etc/ducklake.env` with `PGHOST=192.168.1.169`, and an S3 key.

| Flag            | Default                                    | Meaning                                    |
|-----------------|--------------------------------------------|--------------------------------------------|
| `-listen`       | `127.0.0.1:8097`                           | Address to serve on                        |
| `-token`        | `$SEMANTIC_MCP_TOKEN`                      | Bearer token clients must send; empty = none |
| `-models`       | `models.yml` next to `server.py`           | Semantic models                            |
| `-schema`       | `marts`                                    | Lake schema the models' tables are in      |
| `-catalog`      | `$SEMANTIC_CATALOG`, else `ducklake:postgres:dbname=ducklake` | DuckLake catalog to attach |
| `-endpoint-url` | `$AWS_ENDPOINT_URL`, else `http://192.168.1.169:3900` | S3 endpoint                     |
| `-state-dir`    | DuckDB's own                               | Where DuckDB keeps extensions and temp files |

## Logs

```sh
journalctl -u semantic -f
```
