# nexus

Ask the server to do things, in a chat. You type "get
https://…/debian.iso into isos", a model works out the call, and the page
shows it as a **card**. Nothing happens until you press **Start**. The card
then follows the download until it ends, and the model is told how it went,
so "did it work?" and "now the arm64 one" work as follow-ups.

- **What it can do** is exactly the tools of mcp-hub's `nexus` profile:
  today url-to-bucket's `download`, plus `list_buckets`, `list_objects`,
  `list_downloads` and `suggest_key`, vigil's six read-only tools
  (system, alerts, jobs, events, runs), duckpipe's (pipes, load history,
  stop all, resume) and lakeshape's (jobs, run a job). `suggest_key`
  finds the folders already holding files named like the one to download
  (`2023-08.zip` next to `uk/crime/2026-08.zip`) and hands the model the
  full key, so the card shows exactly where the file will go.
- **Corrections by reply:** if the key is wrong, or there was nothing to
  suggest and the model asked, reply with a folder or key. The model
  proposes again, and the new card replaces the earlier one for the same
  URL (shown as `superseded`), so there is one card to start. A new capability is a tool added to that profile.
- **Read-only tools run right away.** A tool that changes something is only
  proposed: its arguments are checked against the tool's schema, and it
  becomes a card with **Start** and **Cancel**. **Start** calls it once,
  through mcp-hub, with the conversation's id as the run id.
- **Long jobs:** a tool that returns `{"run_id","topic"}` (url-to-bucket's
  `download` does) starts a job; the card reads relay for that run and
  shows `started`, then `done` (`s3://bucket/key`, size, time) or `failed`
  with the error and **Start again**.
- **Conversations** are kept and listed in the sidebar, and continue where
  they left off.

The design and its reasons are in [SPEC.md](SPEC.md).

## Files

| File          | What it is                                                                |
|---------------|---------------------------------------------------------------------------|
| `main.go`     | `serve` and `check`, the JSON API, the login                              |
| `chat.go`     | One turn: the model, read-only calls, proposals, notes on ended cards     |
| `tools.go`    | The hub profile: tools, the schema check, calls                           |
| `jobs.go`     | Cards: **Start**, **Cancel**, following a job on relay, restarts          |
| `store.go`    | Conversations, and each one's cards, a JSON file each                     |
| `index.html`  | The page. Built into the binary                                           |
| [`../chatapi`](../chatapi) | Shared with analyst: the chat completions API, `models.json` and `prompt.md` |
| [`../mcp-hub/client`](../mcp-hub/client) | Shared with analyst and foreman: connecting to the hub, the schema check, results |
| `config/`     | The server's config: `models.json`, `prompt.md`. Deployed as is           |
| `deploy.sh`   | Run on your Mac: builds the binary and installs it on the server          |
| `install.sh`  | Run on the server by `deploy.sh`: binary, secrets, config, systemd service |

## Deploy

```sh
ONLY="url-to-bucket mcp-hub nexus home" ../deploy-all.sh   # list_buckets, the nexus profile, nexus, its home card
./deploy.sh                                                # later, nexus alone
```

- **The config lives in `config/`** and every deploy installs it in
  `/etc/nexus/`. `install.sh` runs `nexus check` first, with the server's
  secrets: a config that doesn't load stops the deploy; an unreachable hub
  profile or model is a warning.
- **`/etc/nexus.env`** (root only) is written on the first run and left
  alone after: `NEXUS_AUTH`, the page's login, and `MCP_HUB_TOKEN`, copied
  from `/etc/mcp-hub.env`. Relay comes from `/etc/relay-client.env`.
- **Data:** `/var/lib/nexus/conversations/` and `cards/`. Deploys keep them.

After deploying:

- **Page:** `http://192.168.1.169:8102` (login: `sudo grep NEXUS_AUTH /etc/nexus.env`)
- **Logs:** `journalctl -u nexus -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8102/tcp`

| install.sh setting | Default                 | Meaning                                              |
|--------------------|-------------------------|------------------------------------------------------|
| `LISTEN`           | `[::]:8102`             | Where the page listens. `127.0.0.1:8102` keeps it local |
| `EXTRA_ARGS`       |                         | Other flags for `nexus serve`, e.g. `-max-steps 6`   |
| `UI_USER`          | `admin`                 | Basic-auth user (first install only)                 |
| `HUB`              | `http://127.0.0.1:8094` | mcp-hub                                              |

## Models and the prompt

`config/models.json` is in analyst's format: any chat completions API with
tool calls (llama-server by default; Claude through Anthropic's
OpenAI-compatible endpoint, with its key in `/etc/nexus.env`). With more
than one model, a new conversation lets you pick one.

`config/prompt.md` says what nexus can do and how to work: list the
buckets, ask when the bucket isn't clear, one download call per URL, and
that a call is only a proposal. Adding a tool to the profile usually means
a line about it here too.

## Cards after a restart

- A card left `calling` (nexus stopped during the call) comes back
  `failed` ("interrupted"). It may have happened, so it is never called
  again by itself; **Start again** if it didn't.
- A card left `running` is followed again on relay, from the job's first
  event, so a download that ended meanwhile is still shown.

## Run it locally

With mcp-hub (with the `nexus` profile), relay, and a model to talk to:

```sh
go build -o nexus . && cp -r config /tmp/nx
MCP_HUB_TOKEN=… ./nexus check -config /tmp/nx
MCP_HUB_TOKEN=… RELAY_URL=… RELAY_TOKEN=… ./nexus serve -config /tmp/nx -data ./data   # then open http://127.0.0.1:8102
```

| Flag         | Default                 | Meaning                                         |
|--------------|-------------------------|-------------------------------------------------|
| `-listen`    | `127.0.0.1:8102`        | Address of the page                             |
| `-config`    | `config`                | `models.json` and `prompt.md`                   |
| `-hub`       | `http://127.0.0.1:8094` | mcp-hub                                         |
| `-profile`   | `nexus`                 | The hub profile: the tools nexus can use        |
| `-data`      | `data`                  | Conversations and cards                         |
| `-max-steps` | `8`                     | Model calls per message                         |
| `-timeout`   | `5m`                    | Longest a message may take to answer            |
