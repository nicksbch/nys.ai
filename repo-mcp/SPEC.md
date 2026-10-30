# repo-mcp: spec (draft, not approved)

An MCP server that lets agents read this GitHub repo and propose a change
to it as a pull request. It is one Go binary with no `git` and no
checkout: it reads a snapshot of `main` and writes through GitHub's REST
API, with a token scoped to this one repo. Agents reach it through
[mcp-hub](../mcp-hub) as `repo-<scope>__*` tools.

It serves the semantic-feedback loop
([analyst/SPEC-feedback-agent.md](../analyst/SPEC-feedback-agent.md)), the
on-call agents (TODO: "On-call agents that open PRs"), and foreman's "more
context: read-only MCP servers for code and configs" (foreman SPEC, "Where
this goes" #3). Opening the PR goes through foreman's approval step
([foreman/SPEC-actions.md](../foreman/SPEC-actions.md)).

- **Part A** builds the server with the read tools and `check_edits`.
  Nothing it does can change GitHub.
- **Part B** adds `open_pull_request`, once foreman actions exist.

## Scopes

What an agent may read and edit depends on its job, not on the server. A
**scope** is a named pair of path lists, and each scope is its own MCP
endpoint, `/mcp/<scope>`. The hub lists each one as its own server, so a
profile gets exactly the scopes it names:

```json
{"name": "repo-semantic", "url": "http://127.0.0.1:8099/mcp/semantic", "token_env": "REPO_MCP_TOKEN"},
{"name": "repo-oncall",   "url": "http://127.0.0.1:8099/mcp/oncall",   "token_env": "REPO_MCP_TOKEN"}
```

The `modeler` profile gets `repo-semantic`, and an agent whose instructions
come from analyst feedback (text a person typed) can then only ever edit
`semantic/models.yml`, whatever it is talked into.

## Config: `config/repo.json`, installed in `/etc/repo-mcp/`

```json
{
  "repo": "nicksbch/nys.ai",
  "branch": "main",
  "scopes": {
    "semantic": {
      "read":  ["semantic/", "analytics/models/", "analytics/sources.yml"],
      "write": ["semantic/models.yml", "semantic/test_models.py"]
    },
    "oncall": {
      "read":  ["duckpipe/", "preprocessor/", "url-to-bucket/", "lakeshape/", "analytics/", "relay/client/"],
      "write": ["duckpipe/", "preprocessor/", "url-to-bucket/", "lakeshape/", "analytics/"]
    }
  }
}
```

- A path ending in `/` covers everything under it. `write` paths must also
  be under `read`.
- Scope names: `a-z 0-9 -`, up to 20 characters.
- As with the other apps, the config lives in the repo and every deploy
  installs it. `repo-mcp check` validates it and tests the token with one
  read (exit 2 if the config is invalid, 1 if GitHub can't be reached or
  the token is refused).

### Never writable, whatever the config says

These are refused by the code, and `check` fails (exit 2) if a `write`
entry would cover one of them:

| Path | Why |
|------|-----|
| `.github/` | Workflows run on GitHub with the repo's token |
| `deploy-all.sh`, `*/deploy.sh`, `*/install.sh`, `linux-scripts/` | They run as root on the server at the next deploy |
| `*.env`, `*.env.example`, `deploy.env*` | Secrets and settings |
| `go.mod`, `go.sum`, `requirements*.txt`, `pyproject.toml` | New dependencies are a decision for you |
| `repo-mcp/`, `mcp-hub/`, `foreman/` | An agent can't widen its own permissions, prompts or tools |
| `CLAUDE.md`, `TODO.md` | Your instructions and backlog |

## Tools

Every scope has the same tools. Paths are relative to the repo root.

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `list_files` | yes | `{"dir": "duckpipe"}` → the files and directories in it. |
| `read_file` | yes | `{"path", "from_line"?, "lines"?}` → the text with line numbers, the total line count, and the commit it was read at. Default: the first 400 lines. |
| `search` | yes | `{"pattern", "dir"?}` → up to 100 matching lines (`path:line: text`) across the readable paths. `pattern` is a Go regexp. |
| `check_edits` | yes | `{"edits"}` → whether the edits apply and pass the checks below, and the unified diff they make. Changes nothing. |
| `open_pull_request` | **no** | Part B. `{"title", "body", "edits"}` → a branch from `main` with the edits, and a PR. Returns its URL. |

- **Outside the scope** is an error, as if the path didn't exist. `..`,
  absolute paths and symlinks are refused.
- **Annotations:** the first four are `readOnlyHint: true`, so foreman
  offers them in any run. `open_pull_request` isn't, so foreman only calls
  it after your approval. Its `_meta` holds `{"preview": "check_edits"}`,
  which foreman uses to check a proposal and show its diff
  ([actions spec](../foreman/SPEC-actions.md), "Preview").
- **The snapshot:** reads come from a tarball of `main` (one API call),
  held in memory and keyed by `main`'s commit. `main`'s head is checked at
  most every 60 s. The whole repo is about 2 MB. Files over 1 MB, and
  binary files, are listed but not read.

### Edits

```json
"edits": [
  {"path": "duckpipe/load.go", "old": "\tif n > 0 {\n", "new": "\tif n >= 0 {\n"},
  {"path": "analytics/models/new.sql", "old": "", "new": "select 1\n"}
]
```

- Like Claude's `str_replace`: each `old` must occur **exactly once** in the
  file as it is on `main` (after the earlier edits to the same file), and is
  replaced by `new`. An empty `old` creates a file, which must not exist.
  A small model only produces the part it changes, and the arguments read
  like a diff.
- No deletes or renames.
- **Limits:** at most 10 files and 30 edits per change, and at most 64 KB
  of `new` text in all.

### Checks

Run by `check_edits`, and again by `open_pull_request` before anything is
written. Any failure is an error naming the edit, and nothing reaches
GitHub.

1. Every path is writable in this scope, and every edit applies.
2. By file type, on the edited file:
   - `.go`: it parses (`go/parser`), and is already `gofmt`-formatted
     (`go/format`, compared with the result). The message gives the
     formatted text, so a model can copy it.
   - `.yml`, `.yaml`: it parses (`gopkg.in/yaml.v3`, the only extra
     dependency).
   - `.json`: it parses.
   - Anything else: it is valid UTF-8 and not empty.

These catch broken syntax only. That the Go code builds and its tests pass
is checked when you deploy (`deploy-all.sh` vets, tests and builds every Go
app first), and, if you want it earlier, by CI on the PR (see "Open
questions").

## The pull request

- **Branch:** `agent/<scope>/<slug of the title>-<6 hex>`, from the commit
  the edits were checked against. If `main` moved since the proposal, the
  edits are applied to the new `main`; if they no longer apply, the call
  fails and nothing is created.
- **Commits:** one per file through the contents API, with the message
  `(<scope>) <title>`, matching the repo's style.
- **Body:** the caller's `body`, then a footer: "Opened by an agent through
  repo-mcp, scope `<scope>`", and the `X-Run-Id` of the request with a link
  to the foreman run when there is one.
- It only ever creates refs under `refs/heads/agent/`. It never merges,
  never pushes to `main`, and never closes or edits other PRs.
- **Ruleset on GitHub (set up once, by hand):** `main` requires a pull
  request, with you as the only bypass actor, so you can still push to it
  directly. Even a bug in repo-mcp, or a leaked token, can't change `main`.
- Each call publishes `repo-mcp.pr` `ok` (with the URL and scope) or
  `failed` (with the error) on relay.

## Secrets: `/etc/repo-mcp.env` (root only)

- `REPO_MCP_TOKEN`: the bearer token the hub sends. It is written on the
  first install, and mcp-hub's install copies it, as it does for vigil and
  semantic.
- `GITHUB_TOKEN`: a **fine-grained** personal access token for
  `nicksbch/nys.ai` only. Part A needs *Contents: read*. Part B needs
  *Contents: read and write* and *Pull requests: read and write*. No
  *Workflows* permission, so it can't touch `.github/` even if the code
  allowed it. The first install leaves it as a commented line, and `check`
  warns until it is set.

## Running

- `repo-mcp serve [-listen 127.0.0.1:8099] [-config /etc/repo-mcp]`
- MCP streamable HTTP at `/mcp/<scope>`, with the official Go SDK. `GET
  /healthz` needs no token.
- systemd with `DynamicUser=yes`. It keeps no state, so it has no state
  directory.
- `EnvironmentFile=-/etc/relay-client.env`.
- A `repo-mcp` step in `deploy-all.sh` before `mcp-hub`, and its servers in
  mcp-hub's `servers.json`. No home card: it has no UI.

## Files

`main.go` (serve, check), `config.go` (scopes, the never-writable list),
`snapshot.go` (tarball, cache), `github.go` (the REST calls), `edits.go`
(apply, checks, diff), `tools.go`, `config/repo.json`, `deploy.sh`,
`install.sh`, `README.md`.

About 900–1,200 lines of Go plus tests.

## Tests

Against an `httptest` fake of GitHub:

- **Paths:** allowed, outside the scope, `..`, absolute, never-writable
  (refused even when the config lists it, and `check` fails).
- **Scopes:** a scope's endpoint can't read or edit another scope's paths.
- **Edits:** applied once, `old` missing, `old` twice, two edits to one
  file, new file, new file that exists, the limits.
- **Checks:** a Go file that doesn't parse, one that isn't `gofmt`-ed, bad
  YAML, bad JSON, each with the edit named.
- **Snapshot:** one tarball fetch per commit; a new `main` is picked up.
- **PR:** branch name, one commit per file, the footer and run id, `main`
  moved with edits that still apply, `main` moved with edits that don't
  (nothing created), and no request ever names a ref outside `agent/`.

## Not in this spec

- Reading other branches, PRs or comments, and merging.
- Deleting or renaming files.
- Building or testing the code on the server (no Go toolchain there).
- Changes to an open PR. A new proposal opens a new PR; close the old one.

## Open questions

- **CI on agent PRs.** A GitHub Actions workflow on pull requests from
  `agent/*` that runs `go vet` and `go test` for each Go app the PR
  touches, and `semantic`'s tests. You would see red or green on the PR
  before checking it out. It runs on GitHub, with a read-only token and no
  secrets, and agents can't edit it (`.github/` is never writable). I'd add
  it with part B. It is one file, but it's a new piece, so it needs your
  go-ahead.
- **The `oncall` scope.** The config above is an example. Which apps the
  on-call agents may edit, and whether their task types get
  `actions:` at all, is decided when those task types are changed.
- **Model.** Code edits need a model that can read Go well. The local
  Gemma at a 16K context is fine for `models.yml`, but I'd give code task
  types the `claude` model from the start.
