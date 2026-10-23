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
   SPEC, "Where this goes" #2): a repo MCP server with write tools on the
   hub, and an approval step before the PR is opened.

Decided: each answer gets its own rating, and only "bad" ratings start
the agent. The event carries the question, the answer and the tool calls,
so the agent doesn't need to read analyst's conversations. Gap found while
speccing: BSL's tools aren't annotated read-only, so foreman would withhold
them; part 2 fixes that in `semantic/server.py`.

## foreman

### Service configs readable by agents — `idea`

Let foreman agents read the service configs (duckpipe pipes,
`servers.json`, …) so they can refer to them, probably through a read-only
MCP server behind mcp-hub. The first Gemma run on the duckpipe-failure task
could only propose a generic fix because it couldn't see duckpipe's config.
