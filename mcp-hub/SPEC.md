# mcp-hub: spec (approved 2026-10-20)

One MCP endpoint for every service on the server. Each service (vigil,
relay, url-to-bucket, the lakehouse…) serves its own tools over MCP.
mcp-hub reads a list of those servers, joins their tools into one catalogue,
and forwards each call to the server that owns it. Agents connect to the hub
only: one URL, one token. It is one Go binary, both the server and a small
CLI.

mcp-hub has no tools of its own and does not change what a tool does. It
does naming, routing, filtering and logging, and nothing else.

## Why not connect agents to each server directly

Clients such as Claude Code can already hold several MCP servers, so the
hub isn't strictly needed. It earns its place when:

- **Agents run on the server** (routines, `relay listen` scripts) and
  should not each carry a list of URLs and tokens.
- **One token.** Agents get the hub's token. The services' tokens stay on
  the server.
- **Small toolsets.** Every tool description costs context, and an agent
  picks tools better from five than from fifty. A **profile** shows an agent
  only the servers it needs (see below).
- **One audit trail.** Every call is published to relay, so vigil and the
  relay dashboard show what agents did, through which tool, and how it went.

Until there are two or three servers, pointing Claude Code at one server's
`/mcp` directly is fine. The hub can come later without changing any
service.

## Contract for an upstream server

This is what a service does to plug in. It is the part every future MCP
server follows, so it is kept short:

1. **Transport:** MCP Streamable HTTP at `/mcp`, on the service's existing
   HTTP listener. stdio is not supported (see "Out of scope").
2. **Auth:** `Authorization: Bearer <token>`, the service's own token. A
   service that is only reachable on `127.0.0.1` may skip auth.
3. **Tool names:** `snake_case`, `a-z 0-9 _`, no `__` (the hub uses it as
   the separator), and short enough that `<server>__<tool>` fits in 64
   characters.
4. **Tools only.** Resources and prompts are ignored by the hub in v1.
5. **Long jobs return at once.** A tool that starts something slow (a
   download, an ingestion) returns a `run_id` right away and publishes its
   progress to relay. The agent then calls `relay__wait` with that
   `run_id`. No tool call should take longer than the hub's call timeout
   (60 s by default).
6. **Errors the agent should see** (bad input, job failed) are returned as
   a tool result with `isError: true` and a plain-text explanation, not as
   protocol errors.
7. **Optional:** send `notifications/tools/list_changed` when the tool list
   changes. The hub re-reads it on a timer anyway.

The official Go SDK (`github.com/modelcontextprotocol/go-sdk`) covers
points 1, 6 and 7 with `mcp.NewServer` and `mcp.NewStreamableHTTPHandler`.

## Configuration: `/etc/mcp-hub/servers.json`

```json
{
  "servers": [
    {"name": "vigil",         "url": "http://127.0.0.1:8090/mcp", "token_env": "VIGIL_TOKEN"},
    {"name": "relay",         "url": "http://127.0.0.1:8091/mcp", "token_env": "RELAY_TOKEN"},
    {"name": "url-to-bucket", "url": "http://127.0.0.1:8093/mcp", "token_env": "U2B_TOKEN",
     "timeout": "30s"},
    {"name": "lakehouse",     "url": "http://127.0.0.1:8095/mcp"}
  ],
  "profiles": {
    "ops":       {"servers": ["vigil", "relay"]},
    "analytics": {"servers": ["lakehouse"]},
    "fetch":     {"servers": ["url-to-bucket", "relay"],
                  "deny":    ["relay__publish"]}
  }
}
```

| Field                | Rules                                                                      |
|----------------------|----------------------------------------------------------------------------|
| `servers[].name`     | Required, unique. `a-z 0-9 -`, up to 20 characters. It is the tool prefix. |
| `servers[].url`      | Required. The upstream's Streamable HTTP endpoint.                         |
| `servers[].token_env`| Optional. Name of the environment variable that holds the upstream token. The values live in `/etc/mcp-hub.env`, never in this file, so it can be read and copied safely. |
| `servers[].timeout`  | Optional. Per-call timeout, default `60s`, up to `10m`.                    |
| `profiles.<name>`    | `a-z 0-9 -`. `servers` lists which servers it shows. Optional `allow` and `deny` hold hub tool names, and may end in `*` (`vigil__get_*`). With `allow`, only matching tools are shown. `deny` is applied after `allow`. |

The config is read at startup and checked strictly: unknown fields, unknown
server names in a profile and a missing `token_env` variable all stop the
hub from starting. A change takes effect on restart
(`systemctl restart mcp-hub`). It is not reloaded live.

## Naming

A hub tool is named `<server>__<tool>`, for example `vigil__recent_events`
or `lakehouse__describe_model`. The double underscore is used because the
Claude API only allows `a-z A-Z 0-9 _ -` in tool names, so a dot won't do.
The tool's description, input schema, output schema and annotations
(`readOnlyHint` and so on) are passed through unchanged.

An upstream tool whose hub name is invalid or longer than 64 characters is
left out, and the hub logs why. It never renames or shortens tools on its
own.

## Endpoints

| Method & path       | Auth      | What it does                                                         |
|---------------------|-----------|----------------------------------------------------------------------|
| `GET /`             | none      | Plain-text usage, the profiles and the state of each upstream        |
| `GET /healthz`      | none      | `ok` when the hub is running, even if some upstreams are down        |
| `/mcp`              | hub token | MCP Streamable HTTP, every tool of every server (no filtering)       |
| `/mcp/{profile}`    | hub token | MCP Streamable HTTP, that profile's tools only                       |
| `GET /v1/servers`   | hub token | `[{"name","url","state":"up"\|"down","tools":N,"last_error","checked"}]` |
| `GET /v1/tools`     | hub token | `[{"name","server","description","profiles":[…]}]`                    |

The hub token is `Authorization: Bearer $MCP_HUB_TOKEN`. One token covers
every profile in v1. A profile is a way to keep the toolset small, not a
security boundary.

Connecting Claude Code from the Mac:

```sh
claude mcp add --transport http hub-ops http://192.168.1.169:8094/mcp/ops \
  --header "Authorization: Bearer $MCP_HUB_TOKEN"
```

## How it works

**Upstream sessions.** At startup the hub opens one MCP client session to
each upstream, in parallel, and calls `tools/list` (every page). Each
upstream is in one of two states:

- `up`: connected, with its tools cached.
- `down`: never reached, or the session failed. Its tools are removed from
  every profile and the hub retries every 30 s. A down upstream never stops
  the hub from starting or the other servers from working.

The tool list is re-read on `notifications/tools/list_changed` and on a
timer every 5 minutes as well.

**Serving.** The hub runs one MCP server per profile, plus one for plain
`/mcp`. When an upstream's tools change, the hub adds or removes tools on
those servers, and the SDK sends `list_changed` to the agents that are
connected, so they see the new list.

**A call.** The hub strips the prefix, finds the upstream session and calls
`tools/call` with the arguments unchanged and that server's timeout. The
upstream's result is returned unchanged. When the hub itself can't complete
the call, it returns a tool result with `isError: true` and a plain-text
reason the agent can act on:

- `vigil is down: dial tcp 127.0.0.1:8090: connection refused`
- `url-to-bucket did not answer within 30s`

A tool the profile doesn't show is rejected as unknown, exactly as if it
didn't exist.

**No retries.** Some tools change things (`url-to-bucket__download`), so a
failed call is reported and never sent again by the hub. The agent decides.

## Audit: relay topic `mcp-hub.call`

Every call publishes one event once it finishes, through `relay/client` in
the background (`bus.Go`). If relay is down, the call still works and the
event is lost.

```json
{
  "topic": "mcp-hub.call",
  "type": "ok",
  "data": {"profile": "fetch", "server": "url-to-bucket", "tool": "download",
           "name": "url-to-bucket__download", "duration_ms": 412, "args": "{\"url\":\"https://…\",\"bucket\":\"isos\"}"}
}
```

- `type` is `ok` or `failed`. `failed` covers both the hub's own errors and
  results the upstream marked `isError`, with `data.error` holding the text,
  as in vigil.
- `args` is the arguments as JSON text, cut to 2 KB. The result is not
  logged: it can be large, and the upstream publishes its own events.
- `profile` is `""` for plain `/mcp`.

- `name` is the hub tool name, which vigil shows as the event's name.
  (Added while building, 2026-10-20.)

vigil can then count `mcp-hub.call` events and alert on failed ones like
any other app, with no change to vigil. Each failed call is a failed
`mcp-hub/call` job to vigil, so it also sends a webhook notice, at most once
every 15 minutes.

## CLI

```sh
mcp-hub serve [-listen [::]:8094] [-config /etc/mcp-hub/servers.json]
mcp-hub check [-config …]     # validate the config, connect to each upstream, print its tools
```

`check` reads the same config and the same environment as `serve`, prints
one JSON line per server (`name`, `state`, `tools`, `error`), and exits 0
if the config is valid and every upstream answered, 1 if some upstream is
down, and 2 if the config is invalid. It is what `install.sh` runs after a
deploy and what you run after editing the config.

## Deploy

`deploy.sh` and `install.sh` follow relay's pattern:

- A systemd service with `DynamicUser=yes` and the same hardening, listening
  on `[::]:8094`. The hub keeps no state, so there is no `/var/lib`
  directory.
- The first run writes `/etc/mcp-hub.env` (root only) with a generated
  `MCP_HUB_TOKEN`, plus a commented-out example of an upstream token. It
  also writes `/etc/mcp-hub/servers.json` with no profiles. Later runs leave
  both alone.
- vigil is set up for you (added while building, 2026-10-20): if
  `/etc/vigil.env` has a `VIGIL_MCP_TOKEN`, it is copied into
  `/etc/mcp-hub.env` whenever it is missing there, and a first
  `servers.json` lists vigil. Without it, `servers.json` starts with no
  servers.
- The `mcp-hub` binary goes to `/usr/local/bin`, then `mcp-hub check` runs
  and its output is shown. A failed check is a warning, not a failed
  deploy, because an upstream may simply not have `/mcp` yet.
- Changed 2026-10-21: `servers.json` now lives in the repo
  (`mcp-hub/servers.json`) and every deploy installs it, so the server's
  config is never edited by hand. `mcp-hub check` runs on the new file
  first, with the server's tokens: a config that doesn't load (exit 2)
  stops the deploy before the binary or the config is replaced; a server
  that is down (exit 1) is still only a warning. This replaces the
  first-run `servers.json` above.
- A `mcp-hub` step is added to `deploy-all.sh` after `url-to-bucket` and
  before `home`.

## Out of scope for v1

- **stdio upstreams.** The hub does not start processes. A stdio-only
  server (the Python BSL server for the lakehouse, say) runs as its own
  systemd unit behind a stdio-to-HTTP bridge and is listed by URL.
- **Resources, prompts, sampling and elicitation**, forwarded from
  upstreams. Tools only.
- **Separate tokens or users per profile**, OAuth, and TLS (LAN only, like
  the other services).
- **Self-registration** of servers, and live config reload.
- **Changing what passes through:** rewriting descriptions, caching
  results, rate limiting, retries.
- **A web page.** `GET /` is plain text, and `/v1/servers` is there for
  `home` or vigil to use later if wanted.

## Size

About 500–700 lines of Go plus tests: `main.go` (flags, `serve`, `check`),
`config.go` (loading and validation), `upstream.go` (sessions, refresh,
up/down), `hub.go` (profiles, naming, forwarding, audit), plus `usage.txt`,
`README.md`, `deploy.sh` and `install.sh`. Dependencies are the MCP Go SDK
and `relay/client` (via `replace relay => ../relay`, like url-to-bucket).

## Tests

These use in-process fake upstreams built with the same SDK.

- Config: valid file accepted, and each kind of mistake rejected with a
  clear message (duplicate name, bad characters, unknown server in a
  profile, missing `token_env` variable, bad timeout).
- Naming: prefixing, too-long and invalid names left out, `allow`/`deny`
  with `*`.
- Routing: a call reaches the right upstream with the arguments and token
  unchanged, and its result comes back unchanged.
- Failures: an upstream down at startup, one going down and coming back
  (tools disappear, then reappear, with `list_changed` sent to the agent),
  a call timing out, a tool hidden by a profile rejected as unknown.
- Audit: one `ok` or `failed` event per call with the right fields, `args`
  cut at 2 KB, and a call still working when relay is down.
- `check`: exit codes 0, 1 and 2.

## Decisions

1. **Port.** Decided: 8094. The lakehouse MCP server would take 8095.
2. **The MCP Go SDK.** Decided: the hub and each service's `/mcp` use
   `github.com/modelcontextprotocol/go-sdk`. Speaking MCP both ways by hand
   would be most of the work and where the bugs would be.
3. **Logging `args`.** Decided: logged, cut to 2 KB. Knowing what an agent
   actually did is the point of the audit trail. The cost is that a secret
   inside an argument (a download link with a key in it, for example) stays
   in relay for its retention period.
4. **Plain `/mcp` with every tool.** Decided: kept, unfiltered, next to the
   profiles.
5. **Which service goes first.** Decided: vigil. It is read-only and easy
   to check against its dashboard.
