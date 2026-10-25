# lakeshape MCP: spec (approved 2026-10-25)

lakeshape serves its own MCP tools, so an agent can see the jobs and run
one now, through [mcp-hub](../mcp-hub/SPEC.md) as `lakeshape__*`. First
user: [nexus](../nexus/SPEC.md) ("rebuild the marts now").

## The tools

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `list_jobs` | yes | `{}` → each job in `jobs.yml`: name, schedule, `on:` triggers, `select`/`exclude`, `full_refresh`, its last run (from `run_history`, as `lastRuns` reads it), and whether it is running now. |
| `run_job` | no | `{"job", "full_refresh"?}` → what `lakeshape trigger` does: publishes `requested` on `lakeshape.trigger` with a new `run_id`, and returns `{"run_id", "topic": "lakeshape.run"}` at once. |

- **`run_job` is a long tool**, as url-to-bucket's `download` is: the
  service runs the job alone with that `run_id` (`runAll` keeps a
  triggered request out of the groups), and reports `started`, then `ok`
  or `failed` on `lakeshape.run`. nexus's card and foreman follow that.
  `_meta: {"wait": "2h"}`.
- An unknown job is a tool error, and nothing is published. The job list
  is the one the service has loaded, so a job just added to `jobs.yml`
  that the service refused to load isn't offered.
- `run_job`'s description says a `full_refresh` rebuilds incremental
  models from scratch and can take much longer.
- The per-model events (`lakeshape.model`) aren't followed by the card:
  `ok` or `failed` on the run is the outcome, and the error names the
  model that failed.

## Serving

lakeshape's service has no listener today. It gets one, for `/mcp` and
`/healthz` only:

- `lakeshape serve -listen 127.0.0.1:8103` (empty for none, the default
  for the CLI on your Mac). Port 8103 is the next free one after nexus.
- **Auth:** `Authorization: Bearer $LAKESHAPE_MCP_TOKEN`. Without a token,
  `/mcp` isn't served.
- `install.sh` writes `LAKESHAPE_MCP_TOKEN` on the first run and sets
  `-listen` in the unit.
- `run_job` publishes with the service's own relay client. Without relay
  the tool is a tool error ("relay is not configured"), as `trigger` is.

## Hub and nexus config

- `mcp-hub/servers.json`: a `lakeshape` server at
  `http://127.0.0.1:8103/mcp`; the `nexus` profile gets the two tools.
- `mcp-hub/install.sh`: `lakeshape` added to the apps whose tokens it
  copies.
- `nexus/config/prompt.md`: a line per tool.

## Code

- `mcp.go` (new): the listener, the tools, the bearer check. About 120
  lines.
- `serve.go`: the publish of `triggerJob` split out (`requestRun`) so the
  tool shares it; the loaded jobs, last runs and "running now" kept on a
  `board` the tools read, since the service's loop owns the jobs.
- `main.go`: the `-listen` flag, for `serve` only.
- `install.sh`, `README.md`.

New dependency: the MCP Go SDK.

## Tests

- `list_jobs` lists the jobs of `testdata/shop/jobs.yml` with their last
  runs.
- `run_job` publishes one `requested` event with the job and
  `full_refresh`, and returns its `run_id`; an unknown job is a tool error
  and publishes nothing.
- `/mcp` needs the bearer token.
