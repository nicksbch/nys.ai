# foreman: spec (approved 2026-10-21)

Always-on agents for the server. Something happens (a relay event, a
schedule, or you asking), foreman turns it into a **task**, and an
**agent** works on it. The agent is a model with the tools of one
[mcp-hub](../mcp-hub/SPEC.md) profile. foreman runs the loop, enforces the
limits, keeps a full record, and puts the result in front of you to review.
It is one Go binary with a built-in web page.

The first use: **a duckpipe ingestion fails → a task is created → an agent
reads the error, works out why it failed, and proposes a fix → you review
it.** In v1 agents only read and explain. Later versions let agents create
tasks for other agents and open pull requests (see "Where this goes"). The
v1 model is shaped so those fit without a redesign.

## Why not Claude Code headless or a hosted agent service

- **`relay listen … -- claude -p …`** works today with no code, and is a good
  way to try a prompt. But it only runs Claude, starts a heavy process per
  event, and leaves nothing to review beyond its output.
- **Hosted agent platforms** run the loop on the provider's side, but the
  tools are on the LAN (mcp-hub), which a hosted sandbox can't reach. The
  loop has to run on the server.
- **A local model has no headless agent CLI** that is reliable enough to run
  unattended. foreman gives local and hosted models the same loop, tools
  and record, so they can be compared on the same task.

## Concepts

| Thing           | What it is                                                         | Where                             |
|-----------------|--------------------------------------------------------------------|-----------------------------------|
| **Agent**       | *Who* does the work: a model, a hub profile, limits, and a general prompt | `/etc/foreman/agents/<name>.md` |
| **Task type**   | *What* to do and *when*: a trigger, which agent, instructions, and the fields of the result | `/etc/foreman/tasks/<name>.md` |
| **Task**        | One piece of work created from a task type: "duckpipe pipe `orders` failed at 02:10" | foreman's data |
| **Run**         | One attempt by an agent at a task, recorded step by step. A task usually has one run, and more if you run it again | foreman's data |

Keeping *who* apart from *what* is what lets the same agent take many
kinds of task, lets you compare two agents on the same task, and later lets
an agent hand a task to another agent.

Names of agents and task types: `a-z 0-9 -`, up to 40 characters, taken
from the file name.

## Agent file

```markdown
---
description: On-call engineer for the server
profile: ops                    # mcp-hub profile whose tools it gets
model: local                    # a name from models.json
max_steps: 10                   # default 10, at most 50
timeout: 5m                     # per run; default 5m, at most 30m
---
You are the on-call engineer for a small home server running garage (S3),
duckpipe (ingestion into a DuckLake lakehouse), url-to-bucket, relay and
vigil. You investigate with your tools before concluding. You say plainly
when the tools can't tell you something, instead of guessing.
```

## Task type file

```markdown
---
description: Explain a failed duckpipe ingestion and propose a fix
on_event: duckpipe.* failed     # relay topic filter, then types (comma-separated)
agent: oncall
group_by: topic data.pipe       # one open task per pipe, not one per failure
output: explanation fix         # the fields of the result
---
A duckpipe ingestion failed. Its relay event is below; data.error holds the
error text.

1. Read the error. Use vigil's tools to see whether this pipe failed before
   and whether anything else on the server is wrong at the same time.
2. Work out the most likely cause.
3. Submit:
   - explanation: what failed and why, in a few plain sentences.
   - fix: the change that would fix it, as concretely as you can (a
     command, a config line, a code change), and how to check it worked.
```

| Key           | Rules                                                                    |
|---------------|--------------------------------------------------------------------------|
| `description` | Optional. Shown in the UI and used in each task's title.                 |
| `on_event`    | A relay topic filter (relay's syntax), a space, and a comma-separated type list. |
| `every`       | A Go duration of at least `1m`. A scheduled task is created on each tick. |
| `agent`       | Required. The agent that works on these tasks.                           |
| `group_by`    | Optional. Event fields (`topic`, `type`, `run_id`, `data.<key>`) that make up the grouping key. See "Grouping". |
| `output`      | Optional. Names of the result's fields (`a-z 0-9 _`), all text. Default: `answer`. |

A task type has `on_event`, `every`, both, or neither. With neither, tasks
are only created from the UI. Front matter in both kinds of file is flat
`key: value` lines: no YAML library, no nesting. Unknown keys are an error.

## How a run is put together

- **System prompt:** the agent's prompt, then the task type's instructions.
  Both are fixed, so this part is identical for every task of the type,
  which is what makes prompt caching work.
- **First user message:** the only part that changes. It is the trigger:
  - for an event: `Event:` and the relay event as JSON,
  - for a schedule: the time and the previous task's outcome,
  - from the UI: the text you typed.
- **Tools:** the read-only tools of the agent's profile (see "Tools"), plus
  one tool foreman adds, `submit`.
- **`submit`** takes the task type's `output` fields, all required, as
  strings. Calling it ends the run `ok` and stores those fields as the
  task's result. This works the same way on every model, gives the UI
  separate fields to show (explanation, fix), and later gives other agents
  a structured result to build on.
  - If the model stops without calling `submit`, foreman sends one reminder
    ("Call submit with your result."). If it stops again, the run ends
    `failed`, with the model's text kept in the transcript.
  - `submit` is offered like any tool, never forced: the Claude models in
    use reject forced tool choice, and local models don't need it.

## Grouping

A failing job usually fails again, every poll or every night. Without
grouping, each failure would create a task and a run: noise, and cost on a
hosted model.

With `group_by`, foreman computes a key from the event, for example
`duckpipe.ingest|orders`. If a task with that key is still open (not
resolved or dismissed), the event is **attached** to it and no new run
starts. The task shows "seen 5 times, last at 04:10", and its events are
listed. Once you resolve or dismiss the task, the next matching event
creates a new one. Without `group_by`, every event creates a task.

## Task lifecycle

```
created ─► running ─► answered ─► resolved
              │          │    └─► dismissed
              │          └─► running   (you ran it again)
              └─► failed ─► running   (ran again)
                     └──► dismissed
```

- **`answered`**: the agent submitted a result. It waits for you.
- **`failed`**: the run hit a limit, was refused, or never submitted. It
  also waits for you.
- **`resolved` / `dismissed`**: you closed it. Resolved means the problem
  is fixed; dismissed means it doesn't matter. Only you close tasks in v1.
- **Run again** starts a new run on the same task. You can choose another
  agent, or another model, so a local and a hosted answer can be compared
  side by side on the same failure.
- **Verdict:** separately from the status, you can mark the result `good` or
  `bad` with a note. That is the record of whether an agent can be trusted
  with a kind of task, and of what to change in its prompt.

A run that couldn't take place, because the model or hub was unreachable,
is kept as `unavailable`, and one cut short by foreman stopping is kept as
`interrupted`. Either way the task goes back to waiting and is run again.
Neither is published as `failed`, since the agent didn't fail. (Added while
building, 2026-10-21.)

Every task records **where it came from**: `event` (with the relay event
id), `schedule`, `user`, and, reserved for later, `agent` with the parent
task. v1 only creates the first three.

## Models: `/etc/foreman/models.json`

```json
{
  "models": {
    "local":  {"api": "openai",    "url": "http://127.0.0.1:8081/v1", "model": "gemma-4-e2b-it",
               "key_env": "LLAMA_API_KEY", "max_tokens": 4096},
    "claude": {"api": "anthropic", "model": "claude-opus-5-5", "effort": "medium",
               "key_env": "ANTHROPIC_API_KEY", "max_tokens": 16000}
  }
}
```

There are two kinds of API, and one loop serves both.

**`openai`: chat completions** (`POST <url>/chat/completions`). This covers
llama-server, Ollama, and hosted services with the same format. Tools are
sent as `function` tools.
- `finish_reason: tool_calls`: run the tools, and send each result back as a
  `tool` message.
- `stop`: the model finished without `submit`, so the reminder rule applies.
- `length`: the run ends `failed`.

It's written with `net/http` and no client library, because the format is
small and providers differ in the details. llama-server must have its chat
template enabled to accept tools (`--jinja` on builds where that isn't the
default), and `foreman check` sends one request with a tool to find out.

**`anthropic`: the Messages API**, through the official Go SDK
(`github.com/anthropics/anthropic-sdk-go`), with a manual loop, because the
tools come from MCP at runtime and every step is recorded and limited.
- **Model:** `claude-opus-5-5` by default. Its effort default is `medium`,
  so `effort` is always sent explicitly from `models.json`. Thinking is
  always on for this model: `thinking` is left unset, and never `disabled`
  or a token budget, which this model rejects.
- **Tool choice:** always `auto`.
- **Stop reasons:**
  - `tool_use`: run the tools and send **all** results in **one** user
    message, with `is_error: true` for failed ones.
  - `end_turn`: no `submit` yet, so the reminder rule applies.
  - `max_tokens`: the run ends `failed`.
  - `refusal`: the run ends `failed`, with `stop_details.category` in the
    error.

  `stop_reason` is always checked before `content` is read.
- **Refusal fallback:** requests opt into the server-side fallback
  (`fallbacks: "default"`, beta `server-side-fallback-2026-07-01`), so a
  declined request is retried on a fallback model chosen by the API. Which
  model answered is recorded. `"fallbacks": false` turns it off.
- **History is append-only:** each response is appended as returned
  (`resp.ToParam()`), thinking blocks included. Nothing earlier is edited.
- **Prompt caching:** tools are sorted by name, and the system prompt is
  fixed (above). Top-level automatic caching is on.
  `cache_read_input_tokens` is recorded per step, so the UI shows whether
  caching works.

Keys come from the environment (`key_env`), in `/etc/foreman.env`.
`models.json` holds no secrets.

## Tools

At the start of each run, foreman opens an MCP session to
`<hub>/mcp/<profile>` and lists its tools. Their hub names
(`vigil__list_jobs`) are valid as-is in both APIs. Descriptions and schemas
pass through.

- **Read-only only (v1):** a tool is offered only if its annotations say
  `readOnlyHint: true`. Others are left out and listed as `withheld` in the
  transcript. So an agent can't change anything, even when its profile
  holds tools that do.
- **Results:** an MCP result becomes text: its text blocks, or its
  structured content as JSON when there is no text. A result over 16 KB is
  cut, with `[cut: N more bytes]` at the end so the model knows. The
  transcript keeps it whole.
- **Linking to the hub's audit trail:** every MCP request carries
  `X-Run-Id: <run_id>`, and mcp-hub copies it into the `run_id` of its
  `mcp-hub.call` events. That is a small hub change (read
  `req.Extra.Header` in its forward handler), included in this work.

**What the duckpipe example can see today:** the error text in the event,
and through vigil's tools, the pipe's history and the server's state. That
is enough to explain most failures, such as a bad file, a schema mismatch,
a full disk or a lake that can't be reached. Proposing a good *code* fix
needs read access to duckpipe's source and config. That would be a future
read-only MCP server, a repo or logs server added to the hub, which needs
no change to foreman.

## Context

Each step resends the whole run, so a long run fills the context. v1 keeps
this simple and visible:
- tool results are cut at 16 KB,
- `max_steps` bounds the run,
- a provider error for a request that is too long ends the run `failed`
  with that reason.

Nothing is summarized or dropped silently. Your llama-server runs with a
16K context (`LLAMA_CTX`), which is small for agents. Raise it if runs fail
for lack of room.

## Running

- **Event triggers:** each task type with `on_event` gets its own relay
  consumer, `foreman-<task type>`, so the server keeps the cursor and
  nothing is missed across restarts. Events are handled one at a time, in
  order.
  - **Acked when handled:** the event attached to an open task, or its new
    task's run ended (`answered` or `failed`; both are results, and both are
    in the UI).
  - **Not acked when foreman couldn't work** (model endpoint unreachable,
    hub down, 5xx): the same event is tried again after 5 s, doubling up to
    5 min, as `relay listen` does. Nothing is skipped silently.
- **Schedules:** a ticker per task type. A tick while that type's previous
  task is still running is skipped and logged.
- **Concurrency:** one run per agent at a time, and at most `-concurrency`
  runs overall (default 2), because a local llama-server serves one
  request at a time well.
- **Limits:** `max_steps` and `timeout` per run, and 3 minutes per model
  call. A model call that fails with 429 or 5xx is retried twice with
  backoff. That retries the request, never the agent's work.

## Events on relay: topic `foreman.<task type>`

| Type        | When                                   | Notes                                          |
|-------------|----------------------------------------|------------------------------------------------|
| `created`   | a task is created                      | `cause` is the triggering relay event          |
| `started`   | a run starts                           | `run_id` is the run                            |
| `ok`        | the run submitted a result             | `data.result` has the result fields, each cut to 4 KB |
| `failed`    | the run ended without a result         | `data.error`                                   |
| `resolved`, `dismissed` | you closed the task        |                                                |

Every event carries `data.task_id`, `data.name` (the task's title), the
agent and model, and for runs the steps, duration and tokens. vigil only
reads `started`, `ok` and `failed`, so each task type appears in vigil's
job table as `foreman/<task type>`, and an `-expect` rule can alert when a
scheduled one stops succeeding. These events are also what later versions
build on: a task type could trigger on another task type's `ok`.

## Storage

- **Transcripts:** `/var/lib/foreman/runs/2026-10-21/<run_id>.jsonl`, one
  record per line, fsynced as the run goes.
  - `start`: task, agent, model, the tools offered and withheld
  - `model`: new messages, the response, stop reason, tokens, cache reads,
    which model answered, duration
  - `tool`: name, arguments, full result, error or not, duration
  - `end`: status, result or error
- **Tasks:** `/var/lib/foreman/tasks.jsonl`, append-only: created, event
  attached, run started and ended, status changed, verdict. The current
  state of every task is rebuilt in memory at startup.
- **Retention:** closed tasks and their runs are deleted after `-retention`
  (default 90 days), at startup and daily. Open tasks are kept.

## Web UI: monitor and review

`GET /ui` serves one embedded page (plain HTML and JavaScript, like
vigil's) behind basic auth (`FOREMAN_AUTH`).

- **Inbox (the default view):** tasks waiting for you, `answered` and
  `failed`, newest first. Each row shows its title, task type, how many
  times it was seen, and the first line of its explanation. It updates
  every few seconds. Filters cover all statuses, task types, agents and
  verdicts.
- **Task:**
  - the trigger, and the attached events when grouped,
  - the result's fields, shown separately (**Explanation**, **Fix**),
  - its runs, each opening to the full transcript: what the model said,
    each tool call with its arguments and result (collapsed when long),
    and the timings, tokens and cache reads,
  - actions: **Resolve**, **Dismiss**, **Good** / **Bad** with a note, and
    **Run again** with an optional other agent or model,
  - a link to the run's `mcp-hub.call` events in relay's dashboard.
- **Agents and task types:** each with its settings, counts (answered,
  failed, verdicts), problems found in its file, and for task types,
  **New task** with input text for trying a prompt.

Agents and task types are edited as files on the server, not in the UI,
so a change is visible in one place and reviewable like the rest of the
config.

## HTTP API

| Method & path                  | Auth       | What it does                                            |
|--------------------------------|------------|---------------------------------------------------------|
| `GET /`                        | none       | Plain-text usage                                        |
| `GET /healthz`                 | none       | `ok`                                                    |
| `GET /ui`                      | basic auth | The web page                                            |
| `GET /api/tasks`               | basic auth | Query: `status`, `type`, `agent`, `verdict`, `limit`    |
| `GET /api/tasks/{id}`          | basic auth | The task, its events, result, runs                      |
| `GET /api/runs/{id}`           | basic auth | A run's whole transcript                                |
| `POST /api/tasks`              | basic auth | `{"type":"…","input":"…"}`: a new task from the UI      |
| `POST /api/tasks/{id}/status`  | basic auth | `{"status":"resolved"\|"dismissed"}`                    |
| `POST /api/tasks/{id}/verdict` | basic auth | `{"verdict":"good"\|"bad","note":"…"}`                  |
| `POST /api/tasks/{id}/run`     | basic auth | `{"agent":"…","model":"…"}` (both optional): run again  |
| `GET /api/agents`, `GET /api/types` | basic auth | Definitions, state, counts                       |

## CLI

```sh
foreman serve [-listen 127.0.0.1:8096] [-config /etc/foreman] [-hub http://127.0.0.1:8094]
              [-data ./data] [-concurrency 2] [-retention 2160h]
foreman check [-config /etc/foreman]   # validate files, reach hub profiles, test each model with one tool call
foreman try <task type> [-input TEXT | -event FILE] [-agent A] [-model M]
                                       # one run in the foreground, transcript on stdout, nothing stored
```

`check` exits 0 when all is valid and reachable, 1 when something is
unreachable, and 2 when a file is invalid. `try` is for writing prompts: it
runs exactly as the service would, against a saved relay event or typed
input, without relay, grouping or storage.

Files are read at startup. A change takes effect on
`systemctl restart foreman`, after `foreman check`.

## Deploy

- A systemd service with `DynamicUser=yes`, state in `/var/lib/foreman`,
  on `[::]:8096`.
- **First run** (later runs leave all of this alone):
  - writes `/etc/foreman.env` (root only) with `FOREMAN_AUTH`,
  - copies in `MCP_HUB_TOKEN` from `/etc/mcp-hub.env` and `LLAMA_API_KEY`
    from llama-server's env file when they exist, and leaves a
    commented-out `ANTHROPIC_API_KEY=`,
  - writes `models.json` with a `local` entry for llama-server, and creates
    empty `agents/` and `tasks/` directories.
- `EnvironmentFile=-/etc/relay-client.env`, as vigil and the hub use.
- A `foreman` step in `deploy-all.sh` after `mcp-hub`, and a `foreman`
  card on `home` (`foreman=8096/ui`).
- Changed 2026-10-21: the config lives in the repo, in `foreman/config/`
  (`models.json`, `agents/`, `tasks/`), and every deploy installs it, so the
  server's config is never edited by hand. It starts with the `oncall`
  agent and the `duckpipe-failure` task type from this spec. `install.sh`
  runs `foreman check` on the new config first, with the server's secrets:
  a config that doesn't load (exit 2) stops the deploy before anything is
  replaced; an unreachable profile or model (exit 1) is a warning.
  `agents/` and `tasks/` are replaced whole, so a file removed from the
  repo is removed from the server. The sample event goes to
  `/etc/foreman/examples/events/` for `foreman try`. This replaces the
  first-run `models.json` and empty directories above.

## Where this goes (not in v1)

Each of these builds on something v1 already has:

1. **Tasks that create tasks.** A task type could trigger on another task
   type's relay events (`on_event: foreman.duckpipe-failure ok`). Or
   foreman could serve its own MCP server with a `create_task` tool, added
   to the hub, so an agent can hand work to another agent. Tasks already
   record their origin, and `agent` + parent task is reserved for this.
2. **Agents that act** (pull requests, re-running a job): this needs a
   repo MCP server with write tools on the hub, and an **approval** step in
   foreman. A run proposes the action, the task waits in the inbox, and the
   tool call only happens after you approve. The read-only rule becomes
   "read-only, or approved by you".
3. **More context:** read-only MCP servers for code, configs and logs. They
   are added to the hub, with no change to foreman.

## Out of scope for v1

- **The three items above.**
- **Memory across tasks** beyond what vigil and relay already hold.
- **Chatting with an agent**, sub-agents in one run, live reload, and
  editing definitions in the UI.
- **Other API formats** than chat completions and the Messages API.
- **Cost in money:** the UI shows tokens.

## Size

About 1,800–2,300 lines of Go plus tests, and about 800 lines of HTML and
JavaScript:
- `main.go`
- `defs.go`: agent and task type files
- `models.go`: the two APIs
- `loop.go`: steps, limits, tools, `submit`
- `tasks.go`: grouping, lifecycle, triggers
- `store.go`: transcripts, the task log
- `server.go`, `ui.html`

Dependencies: the MCP Go SDK, the Anthropic Go SDK, and `relay/client`.

## Tests

These use a fake hub (an in-process MCP server), a fake chat-completions
server and a fake Messages API server, each scripted step by step.

- **Files:** valid agents and task types parse, and each kind of mistake is
  reported.
- **The loop, on both APIs:**
  - tool calls and `submit`: the result fields are stored and the run ends
    `ok`;
  - no `submit`: one reminder, then `failed`;
  - `max_steps`, `timeout`, `refusal`, `max_tokens` and `length` end the
    run `failed`, with the reason recorded;
  - all tool results go back in one message on the Messages API;
  - history is append-only;
  - a 16 KB result is cut with its marker and kept whole in the transcript.
- **Read-only:** a tool without `readOnlyHint` is withheld and listed.
- **Grouping:** a matching event attaches to an open task without a run,
  and a new task is created after resolve or dismiss.
- **Lifecycle:** each transition above, run again with another model, and
  verdicts.
- **Triggers:** acked when handled, not acked when the model endpoint is
  down; a schedule tick during a run is skipped; concurrency.
- **Store:** tasks and transcripts survive a restart; retention keeps open
  tasks.
- **API and auth.**

## Decisions

1. **Port:** 8096. 8095 stays reserved for a lakehouse MCP server.
2. **UI login:** a password (basic auth, `FOREMAN_AUTH`), like vigil's
   dashboard.
3. **Result fields:** text only in v1.
4. **Notifications:** none in v1. The inbox is the only place results
   appear.
