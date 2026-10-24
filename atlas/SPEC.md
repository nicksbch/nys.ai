# atlas: spec (approved 2026-10-24)

A web page that shows how the server's services are wired together: what
writes where, what that wakes up, and which agent can use which tools. It
reads the config files the services actually run with, works out the
links between them, and draws them as one graph. It is one Go binary with
the page built in, like the others.

It is **read-only**: it never writes a file, publishes an event or calls a
service. To act (retry, poll now, run again) you follow its link to the
service's own page, which already does that.

## Why

Every link in the system is implied by two strings that happen to match in
two different files. No file says "A feeds B":

| Link | Made by |
|------|---------|
| url-to-bucket → preprocessor, duckpipe | the bucket and key of a download matching a job's or pipe's `prefix` + `pattern` |
| preprocessor → duckpipe | a job's `to:` prefix matching a pipe's `prefix` + `pattern` |
| duckpipe → lakeshape | a `duckpipe.ingest ok {table}` filter in `analytics/jobs.yml` |
| duckpipe tables → lakeshape models | `analytics/sources.yml` naming the table |
| lakeshape marts → semantic | a `models.yml` model naming a `marts` table |
| any service → foreman | `on_event:` in `foreman/config/tasks/*.md` |
| foreman agents, analyst → tools | agent `profile:` → `servers.json` profile → MCP servers |

When one side changes, nothing says the other side has stopped matching.
atlas makes the links visible, and flags the ones that don't match.

## What it reads

The deployed files on the server, not the repo. The deploy installs the
repo's copies, so they are normally the same; reading the server's files
means the page shows what is really running.

| Service | File(s) on the server | What atlas takes from it |
|---------|-----------------------|--------------------------|
| preprocessor | `/etc/preprocessor.yaml` | `jobs`: name, bucket, prefix, pattern, to |
| duckpipe | `/etc/duckpipe.yaml` | `pipes`: name, bucket, prefix, pattern, table |
| lakeshape | `/etc/lakeshape/project/` `jobs.yml`, `sources.yml`, `models/marts/*.sql` | jobs (name, select, `on` filters, schedule), source tables, mart names |
| semantic | `/opt/semantic/app/models.yml` | each model's name and table |
| mcp-hub | `/etc/mcp-hub/servers.json` | servers (name, url) and profiles |
| foreman | `/etc/foreman/agents/*.md`, `/etc/foreman/tasks/*.md` | agent: profile, model; task: on_event, every, agent |
| analyst | `/etc/analyst/models.json` | its models (its profile is `analyst`) |
| relay | `GET /v1/topics` | every topic and type seen, with its last event |

- **Only the fields it needs:** each file is parsed for the fields above,
  and anything else is ignored. atlas doesn't import the services' Go
  packages (they are separate modules), so a new field in a service's
  config never breaks it. A file that doesn't parse shows as an error on
  its service's node, and the rest of the graph still draws.
- **Never `.env` files:** secrets live in `/etc/*.env`, and atlas has no
  reason or permission to read them. The config files above hold no
  secrets (`servers.json` names its token variables, not the tokens).
- **Re-read on every page load**, at most once every 10 s. The files are
  small, so there is no cache to go stale.
- url-to-bucket has no config: downloads are asked for on relay. Its links
  come from the bucket and key of its `url-to-bucket.download ok` events
  in the last 30 days, and are drawn as *observed* rather than *configured*.

## The graph

Nodes, in columns from left to right, in the order data moves:

1. **Arrivals:** url-to-bucket.
2. **Storage:** S3 prefixes (`home/uk/crime/`, `home/uk/crime/extracted/`),
   with the preprocessor jobs that read and write them.
3. **Loading:** duckpipe pipes, each with its target table.
4. **Building:** lakeshape jobs, the source tables they start from and the
   marts they end in. Models inside a job are not drawn: that is
   lakeshape's DAG, and `lakeshape ls` already shows it.
5. **Serving:** semantic models, and the mcp-hub servers and profiles.
6. **Agents:** foreman task types and agents, and analyst.

Edges have a kind, which sets their style:

| Kind | Drawn as | Example |
|------|----------|---------|
| files | solid | `uk_crime` writes `uk/crime/extracted/` → read by `crime_street` |
| event | dashed, labelled with the filter | `duckpipe.ingest ok {table: lake.uk.crime_street}` → job `crime` |
| table | solid, thin | `uk.crime_street` → source of job `crime` |
| tools | dotted | agent `oncall` → profile `oncall` → server `vigil` |
| observed | solid, faded | url-to-bucket → `uk/crime/` (seen in 4 downloads, 30 days) |

Failure triggers (`on_event: duckpipe.* failed` and so on) would draw a line
from almost every service to foreman. Instead, each node gets a small mark
listing the foreman task types that wake up when it fails, and foreman's
node lists what it listens to. vigil reads every event, so it is shown
once, as a note, with no edges.

### How the links are worked out

- **Prefixes:** a writer's prefix links to a reader whose bucket is the
  same and whose prefix is equal to it, under it, or above it. The
  reader's `pattern` is shown on the edge but not checked: atlas doesn't
  list buckets, and relay events don't name every file an unzip writes.
  Three pipes reading `uk/crime/extracted/` with different patterns are
  three edges from the same prefix.
- **Event filters:** a filter (topic, type, data) links to every service
  whose topics relay has seen and match it, using relay's own rules
  (`a.*`, comma lists). A `data` filter on `table` also links to the pipe
  that loads that table.
- **Tables:** `lake.uk.crime_street`, `uk.crime_street` and the
  `sources.yml` name `uk` + `crime_street` are the same table.

## Live state

Each node and event edge shows its last relay event: type, age, and, for a
failure, the first line of `data.error`. It comes from `GET /v1/topics`,
one call per page load. A `failed` last event colours the node red. The
page refreshes every 30 s while the tab is visible, like home does.

This is a last-event overlay, not a history: for "what happened", the
relay dashboard and vigil remain the places to look, and the node links to
them.

## Problems

A list above the graph of links that don't add up. Each problem names the
file and line it comes from:

- a duckpipe pipe whose prefix no job writes to and no download has
  reached (in 30 days);
- a preprocessor job's `to:` prefix that no pipe reads;
- a lakeshape job filter on a table no pipe loads, or a topic relay has
  never seen;
- a `sources.yml` table no pipe loads;
- a semantic model on a table that isn't a mart;
- a foreman task whose `on_event` matches no topic relay has seen, or
  whose agent doesn't exist; an agent whose profile isn't in
  `servers.json`; a profile naming an unknown server;
- a config file that doesn't parse.

`atlas check` prints the same list and exits 1 if there are any, so it can
run at the end of `deploy-all.sh` next to the status check. Adding it there
is a separate change, for after this is built.

## The page

- **Graph:** the columns above, drawn as SVG by the page itself, with no
  graph library. A fixed column layout reads the same every time, which a
  force layout doesn't.
- **Click a node:** a side panel with its config as it is in the file (the
  YAML block of that pipe, the front matter of that task), the file path
  and line, its last events, and a link to the service's own page. Edges
  into and out of it are highlighted.
- **Filter:** a box that keeps one node and everything up- and downstream
  of it, e.g. type `crime` to see one dataset from download to the agents
  that answer questions about it. Each filter has its own URL.
- **Configs:** a second tab listing every file atlas read, shown whole,
  with its modification time.

## Server

- `atlas serve -listen 127.0.0.1:8101`. Port 8101 (8100 is duckpipe).
- **Password:** basic auth (`ATLAS_AUTH` in `/etc/atlas.env`), as foreman
  does. The config files hold no secrets, but they map the whole server,
  and home stays open because it shows only links. `GET /healthz` stays
  open.
- **API:** `GET /api/graph` returns the nodes, edges, problems and live
  state as JSON; the page draws from it, and so could an agent later.
- **Access:** systemd `DynamicUser`, read-only file system, no extra
  groups: every config above is readable by everyone (see below). The
  install script checks each path is readable and says which isn't.
- **Relay:** a read token from `/etc/relay-client.env`, copied by
  `install.sh` as the other services do. With relay down, the graph still
  draws from the files, with no live state and no observed links.
- **home:** one more card, `atlas=8101`.

## Not in this version

- **Actions** (retry, poll now, run a job): the services' pages do them.
  Adding them here means writes and a second way to do each thing.
- **Editing configs:** config changes go through the repo and a deploy.
- **Comparing the server with the repo** (a config edited but not
  deployed): useful, but it needs the repo on the server or on the page.
- **An MCP tool** over `/api/graph`, for foreman agents (TODO: "Service
  configs readable by agents"). The JSON API makes it easy later.

## Changes to other services

Configs hold no secrets (those are in `/etc/*.env`, `0600`), so every
service's config is made readable by everyone. Most already are:

| Service | Today | Change |
|---------|-------|--------|
| mcp-hub, foreman, analyst, semantic | `0644` / `go=rX` | none |
| preprocessor | `/etc/preprocessor.yaml` `0640 root:preprocessor` | `0644 root:root` |
| duckpipe | `/etc/duckpipe.yaml` `0640 root:duckpipe` | `0644 root:root` |
| lakeshape | project in `/var/lib/lakeshape/project`, `o=` | moves to `/etc/lakeshape/project`, `u=rwX,go=rX`; the unit's `-project` follows |

lakeshape's project has to move: it sits in the service's state directory,
which systemd keeps at `0750` (`StateDirectoryMode`), and the state there
(run history, cursors) should stay private. The install removes the old
`/var/lib/lakeshape/project`. Each change is its own commit, deployed
before atlas.

## Decided

- Name: atlas.
- Configs are made readable by everyone, rather than atlas joining each
  service's group.
- Observed url-to-bucket links: the last 30 days of events.
