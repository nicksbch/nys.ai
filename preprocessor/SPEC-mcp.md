# preprocessor MCP: spec (specced 2026-10-25)

preprocessor serves its own MCP tools, so an agent can see how the unzip
jobs are doing and ask for an archive to be unzipped again, through
[mcp-hub](../mcp-hub/SPEC.md) as `preprocessor__*`. First user:
[nexus](../nexus/SPEC.md) ("did the crime zip unzip?", "unzip it again").

## The tools

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `list_jobs` | yes | `{"job"?}` → each job as the page shows it (`GET /api/state`): name, `s3://bucket/prefix/pattern`, output prefix, last poll and its error, the count of archives by status, and every archive that isn't `ok` (`pending`, `unzipping`, `failed`) with its last attempt's error. |
| `poll_now` | no | `{}` → `POST /api/poll`: the next poll starts as soon as the current one, if any, ends. Returns `{}` at once. |
| `redo_archive` | no | `{"job", "key"}` → `POST /api/redo`: the archive is unzipped again on the next poll (started now). Returns `{"redone": n}`. Entries that haven't changed are skipped, as on the page. |

- **One path for every request.** Each tool calls the code the page's
  handler calls (`view`, `pollNow`, `state.redo`), with the same checks: a
  key that isn't an archive of that job is a tool error, and nothing
  changes.
- **`list_jobs` leaves `ok` archives out** but counts them. A job with
  years of monthly archives would otherwise fill a small model's context
  with the ones that need nothing.
- **Quick tools.** `poll_now` and `redo_archive` return at once; the unzip
  happens on the poll. The descriptions say so, and to call `list_jobs`
  afterwards for the outcome. No `run_id` is returned: the run's id is made
  when the unzip starts, not when it's asked for.
- "Remove record" stays on the page only: it is bookkeeping, not something
  to ask for in a chat.

## Serving

- MCP Streamable HTTP at `/mcp` on the existing listener (`-listen`,
  `127.0.0.1:8099`), with the official Go SDK, as url-to-bucket does.
- **Auth:** `Authorization: Bearer $PREPROCESSOR_MCP_TOKEN` on `/mcp`.
  `guard` passes `/mcp` to the bearer check instead of basic auth and the
  `X-Preprocessor` header, which are for the browser. Without a token,
  `/mcp` isn't served.
- `install.sh` writes `PREPROCESSOR_MCP_TOKEN` into its env file on the
  first run, as url-to-bucket does.

## Hub and nexus config

- `mcp-hub/servers.json`: a `preprocessor` server; the `nexus` profile
  gets the three tools.
- `mcp-hub/install.sh`: `preprocessor` added to the apps whose tokens it
  copies.
- `nexus/config/prompt.md`: a line per tool.

## Code

- `mcp.go` (new): the server, the tools, the bearer check. About 100 lines.
- `ui.go`: `guard` routes `/mcp`; the redo checks split out of `apiRedo`
  so the tool shares them.
- `main.go` (the token from the environment), `install.sh`, `README.md`.

New dependency: the MCP Go SDK.

## Tests

- `list_jobs` returns the counts and only the archives that aren't `ok`;
  `job` filters; an unknown job is a tool error.
- `redo_archive` marks the records redone and pokes a poll; a key that
  doesn't match the job is a tool error and marks nothing.
- `poll_now` pokes a poll; two calls queue one.
- `/mcp` needs the bearer token; basic auth doesn't open it.
