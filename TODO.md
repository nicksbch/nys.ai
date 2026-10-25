# TODO

Ideas and deferred work. An entry here is an idea, not approval: each one
gets a short spec and your go-ahead before it is built.

Status: `idea` → `specced` → `approved` → `done`.

## analyst

### Feedback that improves the semantic layer — `specced`

Specs: [part 1](analyst/SPEC-feedback.md), [parts 2–3](analyst/SPEC-feedback-agent.md), which use [repo-mcp](repo-mcp/SPEC.md) (new) and [foreman actions](foreman/SPEC-actions.md).

A good/bad button on a conversation. The feedback is saved, an event goes to
relay, and a foreman agent picks it up, edits the semantic layer
(`semantic/models.yml`) if that would help, and opens a PR.

It splits into three parts, and each one is useful without the next:

1. **analyst:** good/bad button (plus an optional comment), stored with the
   conversation, and an `analyst.feedback` event published to relay with
   the conversation id.
2. **foreman task type** `on_event: analyst.feedback`: an agent reads the
   conversation and the semantic models and proposes a change to
   `models.yml` (a missing measure, a clearer description, a better
   dimension name), or says no change is needed. This is read-only, so it
   fits foreman v1 as it is.
3. **Opening the PR:** this needs foreman's "agents that act" (foreman
   SPEC, "Where this goes" #2): repo-mcp serves `open_pull_request`
   as its own MCP tool, registered in the hub, and foreman calls it only
   after your approval.

Decided: each answer gets its own rating, and only "bad" ratings start
the agent. The event carries the question, the answer and the tool calls,
so the agent doesn't need to read analyst's conversations. Gap found while
speccing: BSL's tools aren't annotated read-only, so foreman would withhold
them; part 2 fixes that in `semantic/server.py`.

## nexus (new)

### Start downloads from a chat — `approved`

Approved 2026-10-25. Spec: [nexus/SPEC.md](nexus/SPEC.md). A chat page whose model proposes
calls to the tools of one mcp-hub profile; read-only ones run, the others
become cards you **Start**. First and only tool: url-to-bucket's
`download` (plus a new read-only `list_buckets`). Each card follows its
download on relay until it ends.

## mcp-hub

### A token per profile — `idea`

Today one `MCP_HUB_TOKEN` opens `/mcp` and every profile, and analyst,
nexus and foreman all hold it (copied into `/etc/analyst.env`,
`/etc/nexus.env`, `/etc/foreman.env`). A profile keeps a model's toolset
small but isn't a permission: anything with the token can call any tool,
`url-to-bucket__download` included, without foreman's Approve or nexus's
Start. That matters more once repo-mcp's `open_pull_request` is in the
hub. Idea: each profile gets its own token (in `/etc/mcp-hub.env`), a
token only opens its profile, and each service's install copies only the
one it needs. Plain `/mcp` keeps the hub token, for you.

## foreman

### Agents that act — `approved`

Approved 2026-10-25: [foreman actions](foreman/SPEC-actions.md). An agent
proposes a call to a tool its service owns (not read-only), you approve it,
foreman calls it through mcp-hub, and a long job's outcome is followed on
relay. First action tool: url-to-bucket's `download`
([spec](url-to-bucket/SPEC-mcp.md)), on the `url-to-bucket-failure` task
type.

### On-call agents that open PRs — `specced`

Parked 2026-10-23: the four on-call task types stop at suggesting a fix.
They could propose the fix as a PR, opened after your approval. Specced
2026-10-23 as the general case: [repo-mcp](repo-mcp/SPEC.md) (scopes per
job, never-writable paths, `check_edits` with a diff) and
[foreman actions](foreman/SPEC-actions.md) (action tools owned by each
service, approval, preview, replies on relay). Which on-call task types get `actions:`, and the
`oncall` scope's paths, are still to decide. The first action without a
PR: url-to-bucket's own `download` tool
([spec](url-to-bucket/SPEC-mcp.md)), for url-to-bucket failures.

### Service configs readable by agents — `idea`

Let foreman agents read the service configs (duckpipe pipes,
`servers.json`, …) so they can refer to them, probably through a read-only
MCP server behind mcp-hub. The first Gemma run on the duckpipe-failure task
could only propose a generic fix because it couldn't see duckpipe's config.

## atlas (new)

### One page for how the system is wired — `approved`

Spec: [atlas/SPEC.md](atlas/SPEC.md). Specced and approved 2026-10-24; built,
not yet deployed (deploy preprocessor, duckpipe and lakeshape first).

A read-only page that reads the deployed configs (preprocessor, duckpipe,
lakeshape, semantic, mcp-hub, foreman, analyst), works out what feeds and
triggers what (S3 prefixes, relay filters, tables, MCP profiles), and draws
it as one graph with each node's last relay event. It lists the links that
don't match, and `atlas check` prints the same list. No actions: it links to
each service's page. A new service rather than part of home, because it
needs a password and read access to the configs. Decided 2026-10-24:
configs become readable by everyone (preprocessor and duckpipe `0644`,
lakeshape's project moves to `/etc/lakeshape/project`).

### What is running now — `done`

Spec: [atlas/SPEC-running.md](atlas/SPEC-running.md). A Running tab, built
only from relay: a `started` with no `ok`/`failed` for its run ID is
running. It covers downloads, unzips, loads, lakeshape runs (with models
done out of models selected) and foreman tasks. It also lists what is
waiting (`requested` with no `started`) and what ended in the last 15
minutes, and marks the running nodes on the graph. Queued files and
byte-level progress are left out. Built 2026-10-25, not yet deployed.
