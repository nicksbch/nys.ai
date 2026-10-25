# foreman actions: spec (approved 2026-10-25)

foreman's "Where this goes" #2, **agents that act**. An agent is triggered
by an event, observes with its read-only tools, and may decide to **act**:
it proposes a call to a tool that changes something. foreman records the
proposal and doesn't make the call. The call only happens when you
**approve** it in the UI. The rule "agents only get read-only tools"
becomes "read-only tools, or calls approved by you".

```
event ─► foreman task ─► run: observe (read-only tools)
                            └─► proposes url-to-bucket__download {url, bucket}
                                   ─► you approve
                                   ─► foreman calls it through mcp-hub
                                   ─► url-to-bucket returns {run_id, topic} at once
                                   ─► relay: url-to-bucket.download started … ok / failed
                                   ─► foreman: action done / failed, on the task
```

**Each service owns its action tools.** The tool that does something is
served by the app that knows how to do it, over its own `/mcp`, like its
read-only tools: url-to-bucket serves `download`
([spec](../url-to-bucket/SPEC-mcp.md)), repo-mcp serves
`open_pull_request` ([spec](../repo-mcp/SPEC.md)). mcp-hub only registers
and routes them, as it does today, and its audit trail records the call.
foreman holds no tools of its own beyond `submit`.

**relay carries the outcome of long actions.** A tool that starts a slow
job returns its `run_id` at once and publishes the job's progress to relay
(mcp-hub's contract, point 5). foreman follows that `run_id` on relay until
the job ends. A quick tool (opening a PR) returns its outcome directly.

## Task type: `actions`

```markdown
---
on_event: url-to-bucket.download failed
agent: oncall
group_by: topic data.url
actions: url-to-bucket__download    # comma-separated hub tool names
output: explanation fix
---
```

- `actions` lists the tools that are not read-only and that this task type
  may propose. They must be in the agent's hub profile. `foreman check`
  fails (exit 2) if one isn't.
- Actions are set per task type, not per agent. The same agent can then
  have a read-only job and a job that acts, and what a job may do is
  written next to its instructions.
- Tools that are not read-only and not in `actions` are still withheld, as
  in v1.

## During a run

- An action tool is offered with its schema, and its description gets this
  ending: "Calling this proposes the action. It doesn't run now. It runs
  only if a person approves it."
- When the model calls it, foreman checks the arguments against the tool's
  input schema (`github.com/google/jsonschema-go`, already a dependency
  through the MCP SDK), runs its **preview** if it has one (below), stores
  them as the run's **proposed action**, and returns "Proposed. It will run
  after approval. Continue, then call submit." An argument that doesn't fit
  the schema, or a preview that fails, returns an error, so the model can
  correct it.
- A run proposes **at most one action**. A second call returns an error
  ("this run already proposed an action").
- **The run must end `ok`.** A proposal from a run that then fails, hits a
  limit or never submits stays in the transcript, but can't be approved. A
  run that acts has to finish its work.

## What a service declares on an action tool

Everything foreman needs is on the tool itself, so adding an action is a
change to the service that owns it, and none to foreman or the hub. mcp-hub
already passes a tool's annotations and `_meta` through unchanged.

| Where | What | Meaning |
|-------|------|---------|
| annotations | `readOnlyHint` false or unset | It changes something. foreman withholds it unless a task type lists it in `actions`. |
| `_meta.preview` | a tool name on the same server, e.g. `check_edits` | A read-only tool that checks the same arguments without acting (see "Preview"). |
| `_meta.wait` | a Go duration, default `30m`, at most `24h` | For a long tool: how long foreman waits for the job to end on relay. |
| result | structured content `{"run_id": "…", "topic": "…"}` | A long tool: the job started; its `ok` or `failed` comes on relay. Any other result is the outcome itself. |

## Preview

A tool can name a read-only tool on the same server that checks the same
arguments without acting: `_meta: {"preview": "check_edits"}` on repo-mcp's
`open_pull_request`.

- When the action is proposed, foreman calls the preview tool (with the
  hub's prefix, `repo-semantic__check_edits`) with the same arguments,
  through the same session.
- **The preview fails:** the proposal isn't stored, and the preview's error
  goes back to the model, as for a bad argument. An edit that doesn't apply,
  or a Go file that doesn't parse, never reaches your inbox.
- **The preview succeeds:** its text (for repo-mcp, the unified diff) is
  stored with the proposal and shown in the Action box.
- The preview tool must be read-only, or the proposal is refused. A tool
  without `preview` is proposed as before.

## Approving

The task page gets an **Action** box under the result:

- the tool, its short arguments, and the preview: for a PR, a unified diff
  with added and removed lines colored. Without a preview, the arguments,
  with strings that have line breaks shown as blocks,
- **Approve** and **Reject**, each with an optional note,
- once called: the result (a PR URL is linked), or for a long tool, the
  job's relay events as they arrive, with a link to its `run_id` in relay's
  dashboard.

| Action state | Meaning |
|--------------|---------|
| `proposed`   | waiting for you; the task is in the inbox with an "action" mark |
| `calling`    | you approved; foreman is calling the tool through the hub |
| `waiting`    | a long tool started its job; foreman follows its `run_id` on relay, up to `wait` |
| `done`       | the call succeeded, or the job ended `ok`; its result is shown |
| `failed`     | the call returned an error, or the job ended `failed`; shown, and **Approve** can be pressed again |
| `no_reply`   | the job didn't end within `wait`; **Approve** can be pressed again, with a warning that it may still run |
| `rejected`   | you rejected it, with the note |
| `superseded` | **Run again** produced a new proposal; the old one can't be approved |

- **Approve** calls the tool through the hub with the stored arguments as
  they are, and `X-Run-Id` set to the run that proposed it. It goes through
  the same MCP session code as tool calls, within the hub's call timeout
  (60 s). That is why slow work must return a `run_id` at once.
- Approving is the only way to make the call: there is no auto-approve
  setting. Two clicks can't run it twice: the state moves to `calling`
  under a lock first.
- An action's state doesn't change the task's status. You still resolve or
  dismiss the task. For a PR, you resolve it once the PR is merged or
  closed.

## Following a long action on relay

- foreman reads job outcomes through one relay consumer, `foreman-actions`,
  with the filter `*` and the types `ok,failed`. An event whose `topic` and
  `run_id` match a `waiting` action ends it: `ok` → `done` with its `data`,
  `failed` → `failed` with `data.error`. Others are acked and ignored.
- The consumer's cursor is on relay, so a job that ended while foreman was
  down is still matched after a restart.
- Past `wait`, the action becomes `no_reply`. A late `ok` or `failed`
  still records the outcome.
- **Loop guard.** An action's job can fail and trigger the same task type
  (a re-download that fails again). An event whose `run_id` is the job of an
  action this task type sent is attached to that task, open or closed, and
  never starts a new run. An event like that which triggers a *different*
  task type creates a task there, with the origin foreman reserved for
  this: `agent`, and the parent task.

## Trust

mcp-hub has one token for every profile ("a profile is a way to keep the
toolset small, not a security boundary"), so foreman's approval only holds
for foreman's runs. Anything holding the hub token can call an action tool
directly. So an action tool must be one that is acceptable from any hub
client, and the service checks each call itself: url-to-bucket validates
the URL and bucket as for its web page, repo-mcp checks paths against the
scope, and a PR still needs your merge on GitHub. Per-profile tokens are in
"Not in this spec".

## API, storage, events

- `POST /api/tasks/{id}/action` with `{"decision":"approve"|"reject","note":"…"}`.
  It returns `409` if the action isn't `proposed`, `failed` or `no_reply`
  (or isn't `proposed` for `reject`).
- `tasks.jsonl` gets the records `action proposed` (run, tool, arguments,
  preview), `action decided` (decision, note), `action called` (result,
  or `run_id` and `topic` for a long tool), and `action ended` (`ok`,
  `failed` or `no_reply`, with the result or error). The current state is
  rebuilt at startup like the rest.
- **Restarts:** an action left `calling` comes back as `failed`
  ("interrupted"). It is not retried by itself, because it may have
  happened. One left `waiting` keeps waiting.
- The relay topic `foreman.<task type>` gets the types `action_proposed`,
  `action_ok` (with the result, cut to 4 KB), `action_failed`,
  `action_no_reply` and `action_rejected`.
- `GET /api/tasks?action=proposed` gives the inbox filter.

## Changes outside foreman

- **mcp-hub's spec**, the upstream contract: point 5 says what a long tool
  returns (`{"run_id", "topic"}`), and a new point 8 describes action tools
  (not read-only, `_meta.preview`, `_meta.wait`). No hub code changes: it
  already passes annotations and `_meta` through.
- **The first action tools**, each specced on its own:
  - url-to-bucket `download` ([SPEC-mcp.md](../url-to-bucket/SPEC-mcp.md)):
    a long tool; url-to-bucket gets its first MCP server.
  - repo-mcp `open_pull_request` ([spec](../repo-mcp/SPEC.md), part B): a
    quick tool with a preview.

## Code

- `defs.go`: the `actions` key and its check.
- `loop.go`: offer action tools, intercept the calls, the preview, the
  one-per-run rule.
- `actions.go` (new): calling the tool outside a run, the
  `foreman-actions` consumer, `wait`, the loop guard.
- `tasks.go`: the action records and states.
- `server.go`: the endpoint.
- `ui.html`: the Action box (with the diff and the job's events) and the
  inbox mark.
- `README.md`, and an update to `SPEC.md`'s "Where this goes" pointing here.

About 500–700 lines of Go plus tests, and about 150 lines of HTML and
JavaScript. No new dependency.

## Tests

With the fake hub and fake relay foreman's tests already use:

- **The run:** a proposal is stored and not called. A second proposal is
  refused, and so are bad arguments. The preview is called once per
  proposal, its text stored; a failing preview stores nothing and returns
  its error; a preview that isn't read-only is refused. A run that fails
  after proposing leaves nothing to approve.
- **Approval:** approve calls once (the fake hub counts the calls), with
  `X-Run-Id`; two approvals call once; reject doesn't call. A failed call
  can be approved again, and a superseded one can't. A restart while
  `calling` gives `failed`.
- **Long tools:** a `{run_id, topic}` result moves to `waiting`; `ok` and
  `failed` on relay end it; an event on another topic or run is ignored;
  past `wait` gives `no_reply`, and a late outcome is still recorded; a
  restart while `waiting` matches an outcome that arrived meanwhile.
- **Loop guard:** a failed event from the action's job attaches to its task,
  even when closed, and starts no run.
- **`foreman check`** refuses an action that isn't in the profile.

## Not in this spec

- Approvals by anyone but the one basic-auth user, and approval by another
  agent.
- Auto-approval of any kind.
- Per-profile hub tokens, so only foreman's profile can call action tools.
- More than one action per run, or a chain of actions.
- Editing the arguments before approving. Reject with a note, and **Run
  again**, instead.
- Making the call during the run and showing the model the result.
