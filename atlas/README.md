# atlas

One page that shows how the server's services are wired together: what
writes where, what that wakes up, and which agent can use which tools. It
reads the config files the services run with, works out the links between
them, and draws them as one graph, with each node's last relay event. It is
a single Go binary with the page built in.

It is **read-only**: it never writes a file, publishes an event or calls a
service. To act (retry, poll now, run again), follow a node's link to the
service's own page. The design and its reasons are in [SPEC.md](SPEC.md).

- **Graph:** columns in the order data moves: downloads, S3 prefixes,
  preprocessor jobs, duckpipe pipes, lake tables, lakeshape jobs, marts,
  semantic models, then MCP servers, mcp-hub profiles, agents and foreman
  task types.
- **Links:** files (solid), relay events (dashed, with the filter), tables
  (thin), tools (dotted), and url-to-bucket's downloads in the last 30 days
  (faded). Foreman task types that wake up when something fails are a ↯
  mark on that node, not a line, since every service would point at
  foreman.
- **Click a node:** its config as it is in the file (that pipe's YAML
  block, that task's front matter), the file and line, its last event,
  what it comes from and goes to, and a link to the service's page.
- **Filter:** keeps the nodes that match and everything up- and downstream
  of them: `crime` shows one dataset from download to the agents. The
  filter and the selected node are in the URL.
- **Problems:** links that don't add up, each with its file and line. For
  example: a pipe whose prefix nothing writes to, a job waiting for a table
  no pipe loads, a task naming an agent that doesn't exist, a file that
  doesn't parse.
- **Configs:** every file atlas read, shown whole.
- **Live:** each node's newest event from relay's last 1000, refreshed
  every 30 s while the tab is visible. A node whose last event failed is
  red. Without relay the graph still draws, with no live state and no
  observed downloads.

## What it reads

| Service      | On the server                                     |
|--------------|---------------------------------------------------|
| preprocessor | `/etc/preprocessor.yaml`                          |
| duckpipe     | `/etc/duckpipe.yaml`                              |
| lakeshape    | `/etc/lakeshape/project/` (`jobs.yml`, `sources.yml`, `models/marts/*.sql`) |
| semantic     | `/opt/semantic/app/models.yml`                    |
| mcp-hub      | `/etc/mcp-hub/servers.json`                       |
| foreman      | `/etc/foreman/agents/*.md`, `/etc/foreman/tasks/*.md` |
| analyst      | `/etc/analyst/models.json`                        |
| relay        | `GET /v1/topics`, `GET /v1/events?last=1000`      |

Each file is parsed only for the fields that make links; anything else is
ignored, so a new field in a service's config doesn't break atlas. A file
that doesn't parse is a problem, and the rest still draws. Every one of
these files is readable by everyone (they hold no secrets; those are in
`/etc/*.env`, which atlas never reads), so atlas runs with no privileges.

What each lakeshape job builds comes from running `lakeshape ls -select
...` on the project, so the selection rules are lakeshape's own.

## Files

| File          | What it is                                                         |
|---------------|--------------------------------------------------------------------|
| `main.go`     | `serve` and `check`, and running `lakeshape ls`                    |
| `configs.go`  | Reading each service's config                                      |
| `graph.go`    | Working out the links, the problems, the live state and the layout |
| `relay.go`    | Reading topics and events from relay                               |
| `server.go`   | The page, `GET /api/graph`, and the password                       |
| `ui.html`     | The page (plain HTML and JavaScript, built into the binary)        |
| `deploy.sh`   | Run on your Mac: builds the binary and installs it on the server   |
| `install.sh`  | Run on the server by `deploy.sh`: binary, login, systemd service   |

## Run it locally

Against this repo's configs instead of the server's:

```sh
go run . check -repo ..                                   # problems, exit 1 if any
(cd ../lakeshape && go build -o /tmp/lakeshape .)
ATLAS_AUTH=me:pw go run . serve -repo .. -lakeshape /tmp/lakeshape   # http://127.0.0.1:8101
```

Set `RELAY_URL` and `RELAY_TOKEN` for live state.

| Flag         | Default              | Meaning                                                   |
|--------------|----------------------|-----------------------------------------------------------|
| `-repo`      |                      | Read a checkout of the repo, not the server's files       |
| `-lakeshape` | `lakeshape`          | lakeshape binary, for what each job builds                |
| `-listen`    | `127.0.0.1:8101`     | Address to serve the page on                              |
| `-links`     | the services' ports  | The services' pages, as `name=port[/path]`                |
| `-auth`      | `$ATLAS_AUTH`        | `user:pass` for the page; `serve` refuses to start without |

## Deploy to the server

```sh
./deploy.sh                       # or, with everything else: ONLY=atlas ../deploy-all.sh
```

| Variable  | Default         | Meaning                                    |
|-----------|-----------------|--------------------------------------------|
| `HOST`    | `192.168.1.169` | SSH destination                            |
| `SSH_PORT`| `2222`          | SSH port on the server                     |
| `LISTEN`  | `[::]:8101`     | Where the page listens                     |
| `LINKS`   | the services' ports | The services' pages                    |
| `UI_USER` | `admin`         | Login user, first install only             |

The install checks that each config is readable by an unprivileged user,
and ends by running `atlas check`. Deploy preprocessor, duckpipe and
lakeshape first: their installs make their configs readable.

## After deploying

- **URL:** `http://192.168.1.169:8101`, and a card on home.
- **Login:** printed once by the first install; `ATLAS_AUTH` in
  `/etc/atlas.env`.
- **Logs:** `journalctl -u atlas -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8101/tcp`.

## Uninstall

```sh
sudo systemctl disable --now atlas
sudo rm /usr/local/bin/atlas /etc/systemd/system/atlas.service /etc/atlas.env
sudo systemctl daemon-reload
```
