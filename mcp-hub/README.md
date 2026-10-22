# mcp-hub

One MCP endpoint for every service on the server. Each service (vigil today;
url-to-bucket, the lakehouse and others later) serves its own tools over MCP.
mcp-hub reads a list of those servers, joins their tools into one catalogue,
and forwards each call to the server that owns it. Agents connect to the hub
only: one URL, one token.

- **Naming:** each tool appears as `<server>__<tool>`, for example
  `vigil__list_jobs`. Descriptions and schemas pass through unchanged.
- **Profiles:** `/mcp/<profile>` shows only some servers' tools, so an agent
  isn't given fifty tools when it needs five. `/mcp` shows every tool.
- **Down servers:** a server that doesn't answer drops out of the list and is
  tried again every 30 s. The rest keep working. A failed call is reported to
  the agent and never retried, since some tools change things.
- **Audit:** every call is published to [relay](../relay) as `mcp-hub.call`,
  so the relay dashboard and vigil show what agents did and how it went.

The design and its reasons are in [SPEC.md](SPEC.md). What a service does to
plug in is under "Contract for an upstream server" there.

## Files

| File          | What it is                                                           |
|---------------|----------------------------------------------------------------------|
| `main.go`     | `serve` and `check`                                                  |
| `config.go`   | Reading and checking `servers.json`                                  |
| `upstream.go` | One session per upstream server: connecting, tool list, up/down, calls |
| `hub.go`      | Naming, profiles, forwarding, audit, HTTP endpoints                  |
| `usage.txt`   | Usage text, served at `GET /` and printed by `mcp-hub help`          |
| `servers.json` | The server's config: upstream servers and profiles. Deployed as is |
| `deploy.sh`   | Run on your Mac: builds the binary and installs it on the server     |
| `install.sh`  | Run on the server by `deploy.sh`: binary, token, config, systemd service |

## Run it locally

With vigil and semantic running locally (`./vigil -mcp-token vt`, see
[vigil](../vigil); `uv run python server.py serve -token st`, see
[semantic](../semantic)), the repo's `servers.json` works as is:

```sh
go build -o mcp-hub .
VIGIL_MCP_TOKEN=vt SEMANTIC_MCP_TOKEN=st ./mcp-hub check -config servers.json
VIGIL_MCP_TOKEN=vt SEMANTIC_MCP_TOKEN=st MCP_HUB_TOKEN=dev ./mcp-hub serve -config servers.json   # then: curl http://127.0.0.1:8094/
```

## servers.json

```json
{
  "servers": [
    {"name": "vigil",         "url": "http://127.0.0.1:8090/mcp", "token_env": "VIGIL_MCP_TOKEN"},
    {"name": "url-to-bucket", "url": "http://127.0.0.1:8093/mcp", "token_env": "U2B_MCP_TOKEN", "timeout": "30s"}
  ],
  "profiles": {
    "ops":   {"servers": ["vigil"]},
    "fetch": {"servers": ["url-to-bucket", "vigil"], "allow": ["url-to-bucket__*", "vigil__get_run"]}
  }
}
```

| Field                 | Meaning                                                              |
|-----------------------|----------------------------------------------------------------------|
| `servers[].name`      | `a-z 0-9 -`, up to 20 characters. The tool prefix.                   |
| `servers[].url`       | The server's MCP endpoint (Streamable HTTP).                         |
| `servers[].token_env` | Optional. The environment variable holding the server's token, set in `/etc/mcp-hub.env`. Tokens never go in this file. |
| `servers[].timeout`   | Optional. Per-call timeout, default `60s`, at most `10m`.            |
| `profiles.<name>`     | `servers` lists the servers it shows. Optional `allow` and `deny` hold hub tool names, which may end in `*`. `deny` wins. |

The file is checked strictly when the hub starts. A mistake, or a
`token_env` that isn't set, stops it with a message saying what is wrong.
Changes take effect on `sudo systemctl restart mcp-hub`. Run `mcp-hub check`
first.

## Deploy to the server

```sh
./deploy.sh
# or, with everything else:
ONLY=mcp-hub ../deploy-all.sh
```

| Variable     | Default         | Meaning                                                |
|--------------|-----------------|--------------------------------------------------------|
| `HOST`       | `192.168.1.169` | SSH destination                                        |
| `SSH_PORT`   | `2222`          | SSH port on the server                                 |
| `LISTEN`     | `[::]:8094`     | Where the hub listens. `127.0.0.1:8094` keeps it local |
| `EXTRA_ARGS` |                 | Any other flags for `mcp-hub serve`                    |

- **`servers.json` lives in this directory** and every deploy installs it as
  `/etc/mcp-hub/servers.json`. To add a server or a profile, edit it here
  and deploy; don't edit the server's copy, the next deploy replaces it.
  Before replacing anything, `install.sh` runs `mcp-hub check` on the new
  file with the server's tokens. If it doesn't load (bad JSON, an unknown
  server in a profile, a missing token), the deploy stops and the hub keeps
  running as it was. A server that is down is only a warning, since it may
  not serve `/mcp` yet.
- **`/etc/mcp-hub.env`** (root only) is written on the first run and left
  alone after: `MCP_HUB_TOKEN`, the token agents use, and the upstream
  tokens. If vigil has an MCP token, it is copied in as `VIGIL_MCP_TOKEN`,
  also on later runs if it is missing. Tokens are the one thing you add on
  the server, since they don't belong in the repo.

After deploying:

- **Usage and state:** `curl http://192.168.1.169:8094/`
- **Token:** `sudo grep MCP_HUB_TOKEN /etc/mcp-hub.env`
- **Logs:** `journalctl -u mcp-hub -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8094/tcp`

## Connecting an agent

Claude Code, from your Mac:

```sh
claude mcp add --transport http hub http://192.168.1.169:8094/mcp \
  --header "Authorization: Bearer <MCP_HUB_TOKEN>"
# or a profile only:
claude mcp add --transport http hub-ops http://192.168.1.169:8094/mcp/ops \
  --header "Authorization: Bearer <MCP_HUB_TOKEN>"
```

One token opens every profile. A profile keeps an agent's toolset small. It
doesn't limit what the token holder can reach.

## Endpoints

| Method & path     | Auth      | What it does                                          |
|-------------------|-----------|-------------------------------------------------------|
| `GET /`           | none      | Usage, each server's state, the profiles              |
| `GET /healthz`    | none      | `ok` while the hub runs, even if servers are down     |
| `/mcp`            | hub token | MCP, every tool                                       |
| `/mcp/<profile>`  | hub token | MCP, that profile's tools                             |
| `GET /v1/servers` | hub token | `[{"name","url","state","tools","last_error","checked"}]` |
| `GET /v1/tools`   | hub token | `[{"name","server","description","profiles"}]`        |

## The audit trail

Each call publishes one relay event when it finishes:

```json
{"topic": "mcp-hub.call", "type": "ok",
 "data": {"profile": "ops", "server": "vigil", "tool": "list_jobs", "name": "vigil__list_jobs",
          "duration_ms": 12, "args": "{\"app\":\"url-to-bucket\"}"}}
```

- `type` is `failed` when the server returned a tool error or the hub
  couldn't reach it. `data.error` then holds the text.
- `args` and `error` are cut to 2 KB. `profile` is `""` for plain `/mcp`.
- A caller that sends an `X-Run-Id` header (foreman does, with its run's
  id) gets it as the event's `run_id`, so a run's tool calls can be found
  in relay by that id.
- Arguments are logged as sent, so a secret inside one stays in relay for
  its retention period.

vigil reads these like any other app's events (`mcp-hub/call`), so agent
calls show in its job table and a failing tool shows as a failure there.
Each failure also makes vigil send a webhook notice, at most once every
15 minutes.

## Limits

- HTTP servers only. A stdio-only MCP server needs a stdio-to-HTTP bridge
  running as its own service.
- Tools only: resources and prompts of upstream servers are not passed on.
- `servers.json` is read at startup: a deploy installs it and restarts the hub.
- Plain HTTP, like the other services: keep it on the LAN.

## Uninstall

```sh
sudo systemctl disable --now mcp-hub
sudo rm /etc/systemd/system/mcp-hub.service /usr/local/bin/mcp-hub
sudo rm -r /etc/mcp-hub /etc/mcp-hub.env
sudo systemctl daemon-reload
```
