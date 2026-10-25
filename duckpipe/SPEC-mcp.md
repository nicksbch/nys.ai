# duckpipe MCP: spec (approved 2026-10-25)

duckpipe serves its own MCP tools, so an agent can see what the pipes are
loading and what they loaded, and stop or resume loading, through
[mcp-hub](../mcp-hub/SPEC.md) as `duckpipe__*`. First user:
[nexus](../nexus/SPEC.md) ("did the crime CSVs load?", "stop duckpipe").

## The tools

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `list_pipes` | yes | `{}` → what the page polls (`GET /api/state`): the state (`loading`, `maintenance`, `idle`, `stopping`, `stopped`), the batch being loaded, and each pipe's source, table, last poll and its error, and files still to load. |
| `load_history` | yes | `{"pipe"?, "status"?, "limit"?}` → each pipe's totals (files, rows, failed, last load) and the newest load history rows (`GET /api/history`), filtered by pipe and by `loaded` or `failed`. 20 rows by default, at most 500. |
| `stop_all` | no | `{}` → "Stop all" (`POST /api/stop`): the load under way is cancelled and rolled back, and polling pauses until `resume`. Its files load again after `resume`. Returns `{"state"}`. |
| `resume` | no | `{}` → "Resume" (`POST /api/resume`): polling starts again, with a poll right away. Returns `{"state"}`. |

- **One path for every request.** The tools call what the page's handlers
  call (`stop`, `resume`, `stats`, `historyRows`).
- **Stop is for all pipes**, as on the page: duckpipe has no per-pipe stop,
  and this spec doesn't add one.
- **`load_history` runs duckdb** to read the lake, as the page's history
  does. It gets 50 s, inside the hub's 60 s call timeout; a slower answer
  is a tool error ("the lake didn't answer in 50 s").
- `status` filtering is new: `historyRows` gets a status argument
  (`WHERE status = ?`). The page doesn't use it.
- `stop_all`'s description says what stopping costs: the batch in progress
  is rolled back and loaded again later.

## Serving

- MCP Streamable HTTP at `/mcp` on the existing listener (`-listen`,
  `127.0.0.1:8100`), with the official Go SDK, as url-to-bucket does.
- **Auth:** `Authorization: Bearer $DUCKPIPE_MCP_TOKEN` on `/mcp`. `guard`
  passes `/mcp` to the bearer check instead of basic auth and the
  `X-Duckpipe` header. Without a token, `/mcp` isn't served.
- `install.sh` writes `DUCKPIPE_MCP_TOKEN` into its env file on the first
  run.

## Hub and nexus config

- `mcp-hub/servers.json`: a `duckpipe` server; the `nexus` profile gets
  the four tools.
- `mcp-hub/install.sh`: `duckpipe` added to the apps whose tokens it
  copies.
- `nexus/config/prompt.md`: a line per tool.

## Code

- `mcp.go` (new): the server, the tools, the bearer check. About 110 lines.
- `ui.go`: `guard` routes `/mcp`; the state view split out of `apiState`
  so the tool shares it.
- `engine.go`: `historyRows` takes a status.
- `main.go`, `install.sh`, `README.md`.

New dependency: the MCP Go SDK.

## Tests

- `list_pipes` reports `stopped` after `stop_all` and `idle` after
  `resume`.
- `stop_all` cancels the work context under way with `errStopped`.
- `load_history` filters by pipe and status and keeps to `limit`.
- `/mcp` needs the bearer token; basic auth doesn't open it.
