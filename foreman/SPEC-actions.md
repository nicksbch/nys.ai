# foreman actions: spec (draft, not approved)

foreman's "Where this goes" #2, **agents that act**, cut down to what the
semantic-feedback loop needs. An agent may **propose** a call to a tool that
changes something. foreman records the proposal and doesn't make the call.
The call only happens when you **approve** it in the UI. The rule "agents
only get read-only tools" becomes "read-only tools, or calls approved by
you".

## Task type: `actions`

```markdown
---
on_event: analyst.feedback bad
agent: modeler
actions: repo__open_pull_request    # comma-separated hub tool names
output: diagnosis change
---
```

- `actions` lists the tools that are not read-only and that this task type
  may propose. They must be in the agent's hub profile. `foreman check`
  fails (exit 2) if one isn't.
- Actions are set per task type, not per agent. The same agent can then
  have a read-only job and a job that proposes PRs, and what a job may do
  is written next to its instructions.
- Tools that are not read-only and not in `actions` are still withheld, as
  in v1.

## During a run

- An action tool is offered with its schema, and its description gets this
  ending: "Calling this proposes the action. It doesn't run now. It runs
  only if a person approves it."
- When the model calls it, foreman checks the arguments against the tool's
  input schema, stores them as the run's **proposed action**, and returns
  "Proposed. It will run after approval. Continue, then call submit." An
  argument that doesn't fit the schema returns an error, so the model can
  correct it.
- A run proposes **at most one action**. A second call returns an error
  ("this run already proposed an action").
- The run still has to call `submit`. A run that proposed an action and
  then failed keeps the proposal, shown as from a failed run.

## Approving

The task page gets an **Action** box under the result:

- the tool and its arguments. Strings with line breaks are shown as blocks,
  so each `old`/`new` pair of an edit reads like a diff,
- **Approve** and **Reject**, each with an optional note.

| Action state | Meaning |
|--------------|---------|
| `proposed`   | waiting for you; the task is in the inbox with an "action" mark |
| `running`    | you approved; foreman is calling the tool |
| `done`       | the call succeeded; its result (a PR URL) is shown and linked |
| `failed`     | the call returned an error; shown, and **Approve** can be pressed again |
| `rejected`   | you rejected it, with the note |
| `superseded` | **Run again** produced a new proposal; the old one can't be approved |

- **Approve** makes the call through the hub with the stored arguments as
  they are, and `X-Run-Id` set to the run that proposed it. It goes through
  the same MCP session code as tool calls, with a 2-minute timeout.
- Approving is the only way to make the call: there is no auto-approve
  setting. Two clicks can't run it twice: the state moves to `running`
  under a lock first.
- An action's state doesn't change the task's status. You still resolve
  or dismiss the task. For a PR, you resolve it once the PR is merged or
  closed.

## API, storage, events

- `POST /api/tasks/{id}/action` with `{"decision":"approve"|"reject","note":"…"}`.
  It returns `409` if the action isn't `proposed` or `failed`.
- `tasks.jsonl` gets the records `action proposed` (run, tool, arguments),
  `action decided` (decision, note), and `action ended` (result or error).
  The current state is rebuilt at startup like the rest. An action left
  `running` by a restart comes back as `failed` ("interrupted"). It is not
  retried by itself, because it may have happened.
- The relay topic `foreman.<task type>` gets the types `action_proposed`,
  `action_ok` (with the result, cut to 4 KB), `action_failed` and
  `action_rejected`.
- `GET /api/tasks?action=proposed` gives the inbox filter.

## Code

- `defs.go`: the `actions` key and its check.
- `loop.go`: offer action tools, intercept the calls, the one-per-run rule.
- `tasks.go`: the action records and states.
- `server.go`: the endpoint, plus calling the tool outside a run.
- `ui.html`: the Action box and the inbox mark.
- `README.md`, and an update to `SPEC.md`'s "Where this goes" pointing here.

Tests: a proposal is stored and not called. A second proposal is refused,
and so are bad arguments. Approve calls once (a fake hub counts the calls).
Reject doesn't call. A failed call can be approved again, and a superseded
one can't. A restart while `running` gives `failed`. `foreman check`
refuses an action that isn't in the profile.

## Not in this spec

- Approvals by anyone but the one basic-auth user, and approval by another
  agent.
- More than one action per run, or a chain of actions.
- Editing the arguments before approving. Reject with a note, and **Run
  again**, instead.
