# analyst

Ask questions about the lake in a chat. A model answers them with the
[semantic layer](../semantic) only: its tools, through mcp-hub's `analyst`
profile. No SQL, no other MCP server. Every query it runs is shown with its
rows as a table and a chart, and an analysis you like can be saved as a
**report** that runs the same queries again on the latest data.

- **Chat:** conversations are kept, listed in the sidebar, and continue
  where they left off: a follow-up sends the whole conversation to the
  model. Answers stream in as the model works.
- **Queries:** each `query_model` and `compare_periods` call is a card
  with what was asked (model, measures, dimensions, filters, limit), a
  chart, and the rows. Looking around (`list_models`, `get_model`, ...)
  shows as a quiet, collapsed line.
- **Charts:** drawn by the page from the rows: a line when a dimension is
  time, bars otherwise, a second dimension as series. Switch between auto,
  bar, line and table, and pick the measure, on each card.
- **Reports:** **Save as report** picks the conversation's queries that
  worked, with a title and notes. A report stores each tool call as the
  model made it, and the chart as set when saving. Opening a report runs
  its calls again, straight against the semantic layer, without the model:
  the same analysis, every time, on the data as it is now.

## How it works

`analyst serve` serves the page and a JSON API. For each question it
connects to `<hub>/mcp/analyst`, lists the tools whose names start with
`semantic__`, and loops: the model's reply, the tool calls it asks for, their
results back to the model, until it answers in text (at most `-max-steps`
model calls, `-timeout` in all). Each step is streamed to the page and the
conversation saved, so a turn carries on if the page is closed.

- BSL's chart options (`get_chart`, `chart_spec`, ...) are hidden from the
  model and `get_chart` is always false: the page draws the charts, and a
  chart spec would only fill the model's context.
- The model sees a tool result cut at 8 KB (and is told so); the page and
  the stored conversation keep it whole.
- Only a tool call the conversation actually made can go in a report; the
  page names calls by id and the server copies their arguments.

## Files

| File           | What it is                                                               |
|----------------|--------------------------------------------------------------------------|
| `main.go`      | `serve` and `check`, the JSON API, the login                             |
| `chat.go`      | One turn: the model, the tools, the events the page receives             |
| `model.go`     | The chat completions API, `models.json` and `prompt.md`                  |
| `semantic.go`  | The hub's analyst profile: tools, calls, cutting results for the model   |
| `store.go`     | Conversations and reports, a JSON file each                              |
| `index.html`   | The page: chat, query cards, charts (SVG), reports. Built into the binary |
| `config/`      | The server's config: `models.json`, `prompt.md`. Deployed as is          |
| `deploy.sh`    | Run on your Mac: builds the binary and installs it on the server         |
| `install.sh`   | Run on the server by `deploy.sh`: binary, secrets, config, systemd service |

## Deploy

```sh
ONLY="mcp-hub analyst" ../deploy-all.sh   # the hub gets the analyst profile first
./deploy.sh                               # later, analyst alone
```

- **The config lives in `config/`** and every deploy installs it in
  `/etc/analyst/`. Before replacing anything, `install.sh` runs
  `analyst check` with the server's secrets: a config that doesn't load
  stops the deploy; an unreachable hub profile or model is only a warning.
- **`/etc/analyst.env`** (root only) is written on the first run and left
  alone after: `ANALYST_AUTH`, the page's login, and `MCP_HUB_TOKEN`,
  copied from `/etc/mcp-hub.env` (added later if it was missing).
- **Data:** `/var/lib/analyst/conversations/` and `reports/`, a JSON file
  each. Deploys keep them.

After deploying:

- **Page:** `http://192.168.1.169:8098` (login: `sudo grep ANALYST_AUTH /etc/analyst.env`)
- **Logs:** `journalctl -u analyst -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8098/tcp`

| install.sh setting | Default                 | Meaning                                              |
|--------------------|-------------------------|------------------------------------------------------|
| `LISTEN`           | `[::]:8098`             | Where the page listens. `127.0.0.1:8098` keeps it local |
| `EXTRA_ARGS`       |                         | Other flags for `analyst serve`, e.g. `-max-steps 8` |
| `UI_USER`          | `admin`                 | Basic-auth user (first install only)                 |
| `HUB`              | `http://127.0.0.1:8094` | mcp-hub                                              |

## Models

`config/models.json`:

```json
{
  "default": "local",
  "models": {
    "local": {"url": "http://127.0.0.1:8081/v1", "model": "local", "max_tokens": 4096}
  }
}
```

Any chat completions API with tool calls works: llama-server, Ollama, or
Claude through Anthropic's OpenAI-compatible endpoint
(`"url": "https://api.anthropic.com/v1", "model": "claude-opus-5-5",
"key_env": "ANTHROPIC_API_KEY"`, with the key in `/etc/analyst.env`). With
more than one model, a new conversation lets you pick one. A small local
model works but makes more mistakes (a time grain without the time
dimension, an `order_by` on a field it didn't select); the prompt,
`config/prompt.md`, says how to avoid the common ones.

## Run it locally

With mcp-hub (with the `analyst` profile) and a model to talk to:

```sh
go build -o analyst . && cp -r config /tmp/an
# edit /tmp/an/models.json to point at your model, then:
MCP_HUB_TOKEN=… ./analyst check -config /tmp/an
MCP_HUB_TOKEN=… ./analyst serve -config /tmp/an -data ./data   # then open http://127.0.0.1:8098
```

| Flag         | Default                 | Meaning                                         |
|--------------|-------------------------|-------------------------------------------------|
| `-listen`    | `127.0.0.1:8098`        | Address to serve the page on                    |
| `-config`    | `config`                | `models.json` and `prompt.md`                   |
| `-hub`       | `http://127.0.0.1:8094` | mcp-hub                                         |
| `-profile`   | `analyst`               | The hub profile: the only tools the model gets  |
| `-server`    | `semantic`              | Only tools named `<server>__*` are used         |
| `-data`      | `data`                  | Where conversations and reports are kept        |
| `-max-steps` | `12`                    | Model calls per question                        |
| `-timeout`   | `10m`                   | Longest a question may take                     |

`ANALYST_AUTH=user:pass` turns on the login.

## Limits

- One question at a time per conversation. The whole conversation is sent
  with each question, so a long one can outgrow a small model's context;
  start a new conversation then.
- Reports store queries, not answers: the text of the conversation isn't
  rerun. The notes are where to say how to read a report.
- Plain HTTP, like the other services: keep it on the LAN.
