# foreman

Always-on agents for the server. Something happens (a relay event, a
schedule, or you asking), foreman turns it into a **task**, and an
**agent** works on it: a model with the tools of one
[mcp-hub](../mcp-hub) profile. foreman runs the loop, enforces the limits,
keeps a full record of every step, and puts the result in its web page for
you to review.

- **Agents** (*who*): a model, an mcp-hub profile, limits and a prompt.
- **Task types** (*what and when*): a trigger, an agent, instructions and
  the fields of the result.
- **Read-only, or approved by you:** agents are given tools marked
  read-only. A task type can also list **actions**: tools that change
  something, which the agent may only propose. The call is made when you
  approve it on the page.
- **Local or hosted models:** llama-server (or any chat-completions API),
  and Claude through the Messages API.
- **Grouping:** a pipe that keeps failing is one task, "seen 5 times", not
  five.
- **Review:** an inbox of answered and failed tasks, with the full
  transcript of every run, resolve and dismiss, good and bad verdicts, and
  "run again" with another model to compare.

The design and its reasons are in [SPEC.md](SPEC.md).

## Files

| File          | What it is                                                              |
|---------------|-------------------------------------------------------------------------|
| `main.go`     | `serve`, `try` and `check`                                              |
| `defs.go`     | Agent and task type files, `models.json`                                |
| `models.go`   | The two model APIs: chat completions and Anthropic's Messages API       |
| `loop.go`     | One run: tools from mcp-hub, steps, limits, `submit`, proposed actions  |
| `actions.go`  | Approving actions, calling them, following long jobs on relay           |
| `tasks.go`    | Triggers, grouping, the task lifecycle, retries, relay events           |
| `store.go`    | `tasks.jsonl` and the transcripts, retention                            |
| `server.go`   | The JSON API and the password                                           |
| `ui.html`     | The web page (plain HTML and JavaScript, built into the binary)         |
| `config/`     | The server's config: `models.json`, `agents/`, `tasks/`. Deployed as is |
| `examples/`   | Sample relay events, for `foreman try`                                  |
| `deploy.sh`   | Run on your Mac: builds the binary and installs it on the server        |
| `install.sh`  | Run on the server by `deploy.sh`: binary, secrets, config, systemd service |

## Deploy to the server

```sh
./deploy.sh
# or, with everything else:
ONLY=foreman ../deploy-all.sh
```

| Variable     | Default         | Meaning                                                 |
|--------------|-----------------|---------------------------------------------------------|
| `HOST`       | `192.168.1.169` | SSH destination                                         |
| `SSH_PORT`   | `2222`          | SSH port on the server                                  |
| `LISTEN`     | `[::]:8096`     | Where the page listens. `127.0.0.1:8096` keeps it local |
| `EXTRA_ARGS` |                 | Other flags for `foreman serve`, e.g. `-concurrency 1`  |

- **The config lives in `config/`** and every deploy installs it in
  `/etc/foreman/`: `models.json`, `agents/` and `tasks/`. Change agents,
  task types and models here and deploy; don't edit the server's copy, the
  next deploy replaces it (a file removed here is removed there too).
  Before replacing anything, `install.sh` runs `foreman check` on the new
  config with the server's secrets. If it doesn't load (a bad file, an
  agent that doesn't exist, a missing key), the deploy stops and foreman
  keeps running as it was. An unreachable profile or model is only a
  warning.
- **`/etc/foreman.env`** (root only) is written on the first run and left
  alone after: `FOREMAN_AUTH`, the page's login, and `MCP_HUB_TOKEN`,
  copied from `/etc/mcp-hub.env` (added later if it was missing). It gets
  `LLAMA_API_KEY` when llama-server has one, and a commented-out
  `ANTHROPIC_API_KEY=`. Keys are the one thing you add on the server, since
  they don't belong in the repo.
- The sample events go to `/etc/foreman/examples/events/`.

After deploying:

- **Page:** `http://192.168.1.169:8096/ui` (login: `sudo grep FOREMAN_AUTH /etc/foreman.env`)
- **Logs:** `journalctl -u foreman -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8096/tcp`
- **atlas:** has a `foreman` card on its Services tab

## The on-call tasks

`config/` has one agent, `oncall`, and one task type per part of the
pipeline. Each explains a failure and proposes a fix:

| Task type               | Triggered by                     | One open task per |
|-------------------------|----------------------------------|-------------------|
| `url-to-bucket-failure` | `url-to-bucket.download failed`  | URL               |
| `preprocessor-failure`  | `preprocessor.* failed`          | job               |
| `duckpipe-failure`      | `duckpipe.* failed`              | pipe              |
| `lakeshape-failure`     | `lakeshape.* failed`             | (one for lakeshape: a broken run fails several models at once) |

Their tools come from mcp-hub's `oncall` profile, a small set of vigil
tools, which is in [`../mcp-hub/servers.json`](../mcp-hub/servers.json).
Deploying both turns them on:

```sh
ONLY="mcp-hub foreman" ../deploy-all.sh
```

Every matching failure in relay then becomes a task in the inbox. To try a
task type on its sample failure first (it runs exactly like the service and
prints the transcript, but stores nothing), on the server:

```sh
sudo bash -c 'set -a; . /etc/foreman.env; foreman try duckpipe-failure -event /etc/foreman/examples/events/duckpipe-ingest-failed.json'
```

The other samples are `url-to-bucket-download-failed.json`,
`preprocessor-unzip-failed.json` and `lakeshape-model-failed.json`.

## Writing agents and task types

An agent, `config/agents/<name>.md`:

```markdown
---
description: On-call engineer for the server
profile: oncall                 # mcp-hub profile; "all" for every tool
model: local                    # a name from models.json
max_steps: 10                   # default 10, at most 50
timeout: 5m                     # per run; default 5m, at most 30m
---
The prompt: who the agent is and how it works.
```

A task type, `config/tasks/<name>.md`:

```markdown
---
description: Explain a failed duckpipe ingestion and propose a fix
on_event: duckpipe.* failed     # relay topic filter, then event types
every: 24h                      # and/or a schedule (at least 1m)
agent: oncall
group_by: topic data.pipe       # optional: one open task per key
output: explanation fix         # the result's fields (default: answer)
actions: url-to-bucket__download  # optional: tools the agent may propose
---
The instructions for this kind of task.
```

- The agent's prompt and the task type's instructions together make the
  system prompt. The trigger (the relay event, the time, or your text) is
  the first message.
- The agent finishes by calling `submit` with the `output` fields. If it
  stops without doing so, foreman reminds it once.
- A task type with neither `on_event` nor `every` only gets tasks from the
  page's **New task** form.
- After a change, deploy (`ONLY=foreman ../deploy-all.sh`). Its preflight
  runs the tests, which load `config/`, so a broken file is caught before
  anything ships. An agent's profile must exist in
  `../mcp-hub/servers.json`; when you add one, deploy mcp-hub too.

### Actions

An action is a hub tool that isn't read-only, listed in a task type's
`actions`. The service that does the work owns the tool; foreman never
calls it during a run.

- When the agent calls it, foreman checks the arguments against the tool's
  schema and, if the tool names a `preview` in its `_meta` (a read-only tool
  on the same server), runs that and keeps what it says, such as a diff.
  The agent is told "Proposed" and carries on. One action per run.
- Only a run that ends `ok` leaves its proposal on the task, for you to
  **Approve** or **Reject** in its Action box. Approving calls the tool
  through mcp-hub, with the proposing run's id.
- A quick tool's result is the outcome. A long tool returns
  `{"run_id","topic"}` for the job it started; foreman follows its `ok` or
  `failed` on relay (consumer `foreman-actions`) for up to its `_meta`
  `wait` (default 30m), then calls it `no_reply`.
- A failed call, or `no_reply`, can be approved again. A call cut off by a
  restart is marked failed, never retried by itself.
- An event from an action's own job that matches the same task type is
  attached to that task and starts no run, so an action can't start a loop.
- `foreman check` fails (exit 2) on an action that isn't in the agent's
  profile, is read-only, or has a preview that isn't read-only.

Details in [SPEC-actions.md](SPEC-actions.md).

Small local models do much better with few tools and short, explicit
instructions. Keep profiles narrow, and say in the instructions which tool
to call for what.

## Models

`config/models.json` (it has `local` only; this is how to add Claude):

```json
{
  "models": {
    "local":  {"api": "openai", "url": "http://127.0.0.1:8081/v1", "model": "local", "max_tokens": 4096},
    "claude": {"api": "anthropic", "model": "claude-opus-5-5", "effort": "medium",
               "key_env": "ANTHROPIC_API_KEY", "max_tokens": 16000}
  }
}
```

- **`openai`:** any chat-completions API, such as llama-server or Ollama.
  `key_env` is optional; add `"key_env": "LLAMA_API_KEY"` if llama-server
  has a key.
- **`anthropic`:** Claude through the Messages API. Put the key in
  `/etc/foreman.env` as `ANTHROPIC_API_KEY=…` first: a deploy whose
  `models.json` names a key that isn't set stops at the check.
  - A declined request is retried on a fallback model chosen by the API.
    Set `"fallbacks": false` to turn that off. The transcript shows which
    model answered.
  - `effort` is `low`, `medium`, `high`, `xhigh` or `max`.

With a `claude` entry, **Run again** on a task can use it, to compare its
answer with the local model's.

## The page

- **Inbox:** tasks waiting for you (answered or failed), and tasks in
  progress. A task with an action to decide on is marked "action".
- **Task:**
  - the result's fields,
  - **Resolve** (fixed) or **Dismiss** (doesn't matter),
  - a good or bad verdict with a note,
  - **Run again** with another agent or model,
  - the **Action**, when a run proposed one: the tool, its arguments, the
    preview (a diff is colored), **Approve** and **Reject**, and what came
    back,
  - the runs, each opening to its full transcript.
- **All tasks:** filter by status, task type and verdict.
- **Agents & task types:** their settings, counts and prompts, and a
  **New task** form.

## Relay events

Each task type publishes on `foreman.<task type>`:
- `created` when a task is created,
- `started`, then `ok` or `failed`, for each run,
- `resolved` and `dismissed` when you close a task,
- `action_proposed`, then `action_ok`, `action_failed`, `action_no_reply` or
  `action_rejected`, with `run_id` the run that proposed it.

`data.task_id` and `data.name` (the title) are always set. `ok` events
carry `data.result`.

vigil reads `started`, `ok` and `failed`, so each task type shows in its job
table as `foreman/<task type>`. A run that couldn't take place, because the
model or hub was unreachable, is retried and never published as `failed`.

Each run's tool calls are in relay as `mcp-hub.call` events with the run's
id as `run_id`.

## CLI

```sh
foreman serve [-listen 127.0.0.1:8096] [-config /etc/foreman] [-hub http://127.0.0.1:8094]
              [-data DIR] [-concurrency 2] [-retention 2160h]
foreman try <task type> (-input TEXT | -event FILE) [-agent A] [-model M]
foreman check
```

## Run it locally

With relay, vigil and mcp-hub running locally (see their READMEs) and a
model to talk to:

```sh
go build -o foreman . && cp -r config /tmp/fm
# edit /tmp/fm/models.json to point at your model, then:
MCP_HUB_TOKEN=… ./foreman check -config /tmp/fm
FOREMAN_AUTH=admin:dev MCP_HUB_TOKEN=… RELAY_URL=http://127.0.0.1:8091 RELAY_TOKEN=… \
  ./foreman serve -config /tmp/fm -data ./data      # then open http://127.0.0.1:8096/ui
```

## Limits

- Agents act only through a task type's `actions`, one per run, after you
  approve it. Pull requests and agents creating tasks for other agents
  come later; see SPEC.md.
- Each run starts from its trigger alone, with no memory of earlier tasks
  beyond what its tools show.
- The conversation is resent at each step, so a small context fills up.
  Tool results are cut at 16 KB, with a note to the model. If runs fail
  because the context is full, raise llama-server's `CTX`.
- Plain HTTP, like the other services: keep it on the LAN.

## Uninstall

```sh
sudo systemctl disable --now foreman
sudo rm /etc/systemd/system/foreman.service /usr/local/bin/foreman
sudo rm -rf /etc/foreman /etc/foreman.env /var/lib/private/foreman /var/lib/foreman
sudo systemctl daemon-reload
```
