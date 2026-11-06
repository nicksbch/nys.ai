# lake-ui MCP: spec (specced 2026-11-06)

lake-ui serves its own MCP tools, so an agent can browse the lake's tables
and run read-only SQL on them, through [mcp-hub](../mcp-hub/SPEC.md) as
`lake-ui__*`. First user: [nexus](../nexus/SPEC.md), for an operator
investigating data ("what's in uk.onspd?", "how many rows did the
2026-08 file add?", "which postcodes have no lat/long?").

This is for investigating, not analytics: questions about the data's
meaning go through analyst and the semantic layer. The tools see every
schema the page sees, raw tables included.

## The tools

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `list_tables` | yes | `{"schema"?}` → each table's schema, name and number of columns, from the page's `GET /api/tables`, filtered to one schema. No columns, so the answer stays small. |
| `describe_table` | yes | `{"table"}` (`schema.table`) → its columns (name, type) and its row count (`SELECT count(*)`). An unknown table is a tool error naming the schemas there are. |
| `query` | yes | `{"sql", "limit"?}` → `{"columns", "rows", "truncated", "ms"}`, as `POST /api/query` returns, with up to `limit` rows: 100 by default, at most 500. |

- **One path for every request.** The tools call what the page's handlers
  call: the `duckdb_columns()` listing, `wrap` (one statement that returns
  rows, run inside `query()`), `begin` (the 4 query slots) and
  `parseResult`. Whatever the page refuses, the tools refuse, with the same
  message.
- **Sized for a model.** `query` cuts every cell longer than 300
  characters to 300, with `…` at the end, and sets `truncated` when it
  dropped rows or cut a cell. Its description tells the model to `LIMIT`,
  filter or aggregate instead of reading many rows, and that raw tables
  have tens of millions of rows.
- **Time:** a tool call gets 110 s (`-mcp-timeout`), inside the hub's
  timeout for lake-ui (120 s, set in `servers.json`). A slower query is a
  tool error ("the query ran longer than 110s and was stopped"). The
  page's 5 minutes don't change.
- **Same credentials as the page:** the `lake_ui` role and `lake-ui-key`,
  both read-only. Nothing the tools do can change the lake.

## Serving

- MCP Streamable HTTP at `/mcp` on the existing listener (`-listen`,
  `[::]:8092`), with the official Go SDK, as duckpipe does.
- **Auth:** `Authorization: Bearer $LAKE_UI_MCP_TOKEN` on `/mcp`. `guard`
  passes `/mcp` to the bearer check instead of basic auth and the
  `X-Lake-UI` header. Without a token, `/mcp` isn't served.
- `install.sh` writes `LAKE_UI_MCP_TOKEN` into `/etc/lake-ui.env` on the
  first run, and adds it to an existing file that lacks it.

## Hub and nexus config

- `mcp-hub/servers.json`: a `lake-ui` server
  (`http://127.0.0.1:8092/mcp`, `LAKE_UI_MCP_TOKEN`, `"timeout": "120s"`);
  the `nexus` profile gets the three tools.
- `mcp-hub/install.sh`: `lake-ui` added to the apps whose tokens it
  copies, and to `After=`.
- `nexus/config/prompt.md`: the "You can't list or query tables" section
  becomes how to investigate: `list_tables`, then `describe_table`, then
  small `query`s; always `LIMIT`; `staging` holds the cleaned views and
  the raw `<area>` tables keep every release and changed record (count
  raw for "what arrived", staging for "what the data says");
  `duckpipe.load_history`, `lakeshape.run_history`,
  `ducklake_snapshots('lake')` and `AT (VERSION => n)` are queryable too.
  The "where does a table come from" steps stay.
- `nexus/README.md`: lake-ui's tools in the list.

All three tools are read-only, so nexus runs them right away: no cards, no
nexus code changes.

## Code

- `mcp.go` (new): the server, the three tools, the bearer check, the cell
  cut. About 130 lines.
- `main.go`: `guard` routes `/mcp`; `-mcp-timeout`; the listing and the
  query split out of the `tables` and `query` handlers so the tools share
  them.
- `install.sh`, `README.md`.

New dependency: the MCP Go SDK.

## Tests

- `query` refuses what the page refuses (`CREATE`, two statements).
- `query` keeps to `limit`, caps it at 500, cuts long cells and sets
  `truncated` for either.
- `list_tables` filters by schema; `describe_table` errors on an unknown
  table.
- `/mcp` needs the bearer token; basic auth doesn't open it.

## Not in this spec

- Column meanings, grain and release rules (the staging models' `.sql`
  header comments and `.yml` files). Declined for now, 2026-11-06.
