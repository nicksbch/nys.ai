# repo-mcp: spec (draft, not approved)

An MCP server that lets agents read a few paths of this GitHub repo and
propose a change to them as a pull request. It is one Go binary with no
local checkout: everything goes through GitHub's REST API with a token
scoped to this one repo. Agents reach it through [mcp-hub](../mcp-hub) as
`repo__*` tools.

It is built for the semantic-feedback loop
([analyst/SPEC-feedback-agent.md](../analyst/SPEC-feedback-agent.md)), and is
also foreman's "more context: read-only MCP servers for code and configs"
(foreman SPEC, "Where this goes" #3). Any agent whose profile includes it can
read the allowed paths.

- **Part 2** builds the server with the two read tools only.
- **Part 3** adds `open_pull_request`.

## Tools

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `list_files` | yes | `{"dir": "semantic"}` → the files and directories in it, on `main`. |
| `read_file` | yes | `{"path": "semantic/models.yml"}` → the file's text on `main`, and its blob sha. |
| `open_pull_request` | **no** | Part 3. `{"title", "body", "edits": [{"path", "old", "new"}]}` → a branch from `main` with the edits, and a PR. Returns the PR's URL. |

- **Paths are allow-listed.** `read` paths can be read and `write` paths can
  be edited. A path outside the list is an error, as if it didn't exist.
  `..`, absolute paths and symlinks are refused.
- **Read-only tools are annotated** `readOnlyHint: true`, so foreman offers
  them. `open_pull_request` isn't, so foreman only runs it after your
  approval ([foreman/SPEC-actions.md](../foreman/SPEC-actions.md)).
- **Edits work like Claude's `str_replace`:** each `old` must occur exactly
  once in the file on `main`, and is replaced by `new`. An empty `old` means
  a new file. A small model only has to produce the part it changes, not
  the whole file, and the arguments read like a diff when you approve them.
- **Checks before anything is written:** every edit applies, every path is
  in `write`, and a `.yml` file still parses as YAML (`gopkg.in/yaml.v3`,
  the only extra dependency). Any failure returns an error, and nothing is
  created on GitHub.
- **The PR:** branch `agent/<slug of the title>-<6 hex>`, one commit per
  file through the contents API, then the PR against `main`. The body is
  the caller's `body`, followed by a footer: "Opened by an agent through
  repo-mcp", and the `X-Run-Id` of the request when there is one, so the PR
  links back to the foreman run. It never merges, never pushes to `main`,
  and never closes or edits other PRs.
- **Reads are cached for 60 s** per path, so a run that reads `models.yml`
  five times makes one API call.

## Config: `config/repo.json`, installed in `/etc/repo-mcp/`

```json
{
  "repo": "nicksbch/nys.ai",
  "branch": "main",
  "read":  ["semantic/", "analytics/models/", "analytics/sources.yml"],
  "write": ["semantic/models.yml", "semantic/test_models.py"]
}
```

A path ending in `/` covers everything under it. `write` paths must also be
under `read`. As with the other apps, the config lives in the repo and every
deploy installs it. `repo-mcp check` validates it, and tests the token
against the repo with one read (exit 2 if the config is invalid, 1 if
GitHub can't be reached or the token is refused).

## Secrets: `/etc/repo-mcp.env` (root only)

- `REPO_MCP_TOKEN`: the bearer token the hub sends. It is written on the
  first install, and mcp-hub's install copies it, as it does for vigil and
  semantic.
- `GITHUB_TOKEN`: a **fine-grained** personal access token limited to
  `nicksbch/nys.ai`. Part 2 needs *Contents: read*. Part 3 needs
  *Contents: read and write* and *Pull requests: read and write*. The first
  install leaves it as a commented line for you to fill in, and `check`
  warns until it is set.

## Running

- `repo-mcp serve [-listen 127.0.0.1:8099] [-config /etc/repo-mcp]`
- MCP streamable HTTP at `/mcp`, with the official Go SDK (as analyst
  uses). `GET /healthz` needs no token.
- systemd with `DynamicUser=yes`. It keeps no state, so it has no state
  directory.
- `EnvironmentFile=-/etc/relay-client.env`: each `open_pull_request`
  publishes `repo-mcp.pr` `ok` (with the URL) or `failed` (with the
  error).
- A `repo-mcp` step in `deploy-all.sh` before `mcp-hub`, and an entry in
  mcp-hub's `servers.json`.

## Files

`main.go` (serve, check), `github.go` (the REST calls), `tools.go` (the
tools, paths, edits), `config/repo.json`, `deploy.sh`, `install.sh`,
`README.md`, and tests: the path rules (allowed, refused, `..`), edits
(applied once, missing, ambiguous, new file), YAML check, and the GitHub
calls against an `httptest` fake (branch, commits, PR, footer).

## Not in this spec

- Reading other branches or PRs, comments on PRs, and merging.
- A local checkout or `git` on the server.
- Running the semantic tests on the PR. `semantic/deploy.sh` runs them,
  and `server.py check`, before anything goes live (see "Safety" in the
  feedback-agent spec).
