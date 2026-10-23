# Semantic feedback agent: spec, parts 2 and 3 (draft, not approved)

Follows [SPEC-feedback.md](SPEC-feedback.md) (part 1: the 👎 button and the
`analyst.feedback` event). A foreman agent takes each bad rating, works out
why the answer was bad, and says whether the semantic layer
(`semantic/models.yml`) is at fault and what to change. In part 3 it also
proposes that change as a pull request, which is opened only after you
approve it.

```
👎 in analyst ─► relay: analyst.feedback bad ─► foreman task "semantic-feedback"
     agent "modeler": semantic__* (try the question) + repo__read_file (models.yml, marts)
     ─► result: diagnosis, change                                   (part 2)
     ─► proposes repo__open_pull_request ─► you approve ─► PR       (part 3)
     ─► you review and merge ─► semantic/deploy.sh (tests, check) ─► live
```

## Part 2: diagnose and propose, read-only

### What it needs, and the gaps today

| Need | Today | Change |
|------|-------|--------|
| The question, the answer, the calls and errors, your comment | In the part 1 event | none |
| Ask the semantic layer the question again | BSL's tools, but **they aren't annotated read-only, so foreman withholds them all** | `semantic/server.py` marks its tools `readOnlyHint: true` |
| See the expressions in `models.yml`, and the mart columns no model uses yet | `get_model` shows names and descriptions only | the new [repo](../repo-mcp/SPEC.md) MCP server: `read_file`, `list_files` |
| A hub profile with both | `analyst` (semantic only) | new profile `modeler` |

**semantic/server.py:** after the BSL tools are registered, set
`annotations.readOnlyHint = True` on each. It is true: the lake is attached
read-only. Add a test in `test_models.py` that lists the tools and checks
the flag. analyst doesn't look at annotations, so it is unaffected.

**mcp-hub `servers.json`:**

```json
{"name": "repo", "url": "http://127.0.0.1:8099/mcp", "token_env": "REPO_MCP_TOKEN"}
"modeler": {"servers": ["semantic", "repo"]}
```

### foreman config, in the repo

`foreman/config/agents/modeler.md`:

```markdown
---
description: Keeps the semantic layer accurate and easy for analysts to use
profile: modeler
model: local
max_steps: 20
timeout: 10m
---
You maintain the semantic layer of a small lakehouse: semantic/models.yml,
in the Boring Semantic Layer's YAML format, over the marts built from
analytics/models/marts. An analyst agent answers people's questions with
the semantic layer's tools only, so a missing measure, a vague
description or a confusing name leads it to wrong answers.

Base every statement on what a tool showed you. A bad answer is often not
the semantic layer's fault (the question can't be answered from the data,
or the model just made a mistake): say so when that is the case, and
propose no change.
```

`foreman/config/tasks/semantic-feedback.md`:

```markdown
---
description: Learn from an answer a person rated bad
on_event: analyst.feedback bad
agent: modeler
group_by: run_id data.question_index   # re-rating the same answer joins the open task
output: diagnosis change
---
A person rated an analyst answer bad. The event is in the first message:
data.question, data.answer, data.calls (each tool call with its arguments
and error) and data.comment (what the person said was wrong, maybe empty).

1. Read the question, the comment and the calls. Find where it went wrong:
   a call that failed, a measure or dimension that was missing or misread,
   or a correct query answering the wrong thing.
2. Read semantic/models.yml, and the mart's SQL and YAML under
   analytics/models/marts, to see what exists and what the data has.
3. Try the question yourself with the semantic tools, to check your
   explanation.
4. Submit:
   - diagnosis: what went wrong and why, in a few plain sentences, and
     whether the semantic layer is the cause.
   - change: the edit to semantic/models.yml (the exact text to replace,
     and what to replace it with), and why it would have led to a right
     answer. Or "none" with the reason.
```

`foreman/examples/events/analyst-feedback-bad.json`: a sample event, for
`foreman try semantic-feedback -event …`.

### What changes in which app (part 2)

| App | Change |
|-----|--------|
| `semantic` | read-only annotations, and a test |
| `repo` (new) | the server with `list_files` and `read_file` ([spec](../repo-mcp/SPEC.md)) |
| `mcp-hub` | `repo` server, `modeler` profile, and `install.sh` copies `REPO_MCP_TOKEN` |
| `foreman` | config only: the agent, the task type, the sample event. **No code change** |
| `deploy-all.sh`, `home` | a `repo-mcp` step before `mcp-hub`. No home card (no UI) |

The results appear in foreman's inbox. You can apply a proposed change by
hand, and that is already the whole loop, without part 3.

## Part 3: the pull request, after your approval

- **repo:** adds `open_pull_request` ([spec](../repo-mcp/SPEC.md)). The GitHub
  token gets write access to contents and pull requests.
- **foreman:** gets approved actions ([SPEC-actions.md](../foreman/SPEC-actions.md)).
- **mcp-hub:** the `modeler` profile already shows every `repo` tool.
- **The task type** adds `actions: repo__open_pull_request`, and a step 5:

  > 5. If you propose a change, also call repo__open_pull_request with
  >    the same edit. Title: what the change does. Body: the question, the
  >    person's comment, your diagnosis, and how to check the change.
  >    Then submit as above.

The PR body's footer links back to the foreman run. You approve the action
in foreman, and the PR appears on GitHub. You review it there and merge it,
then deploy semantic as usual.

## Safety

The question and the comment are text a person typed. They reach an agent
that can propose a code change. These limits contain that:

1. **Two people-steps:** approving the action in foreman, then merging the
   PR on GitHub. Nothing changes `main` on its own.
2. **Narrow writes:** repo only edits the `write` paths
   (`semantic/models.yml`, `semantic/test_models.py`), on a new branch, with
   a token scoped to this one repo.
3. **Deploy gate:** `semantic/deploy.sh` runs `test_models.py`, then
   `server.py check` against the lake, before replacing anything. A
   `models.yml` that doesn't load never goes live.

## Open questions

- **Which model?** The local Gemma, at a 16K context, has to fit
  `models.yml`, a mart's SQL and the event. It should fit today, with one
  small model, but the diagnosis is the hard part. I'd start with `local`,
  compare it against `claude` with **Run again** on the first few tasks,
  and then decide. That needs a `claude` entry in `foreman/config/models.json`
  and `ANTHROPIC_API_KEY` in `/etc/foreman.env`.
- **Name:** `repo-mcp` for the new app (the binary, the service, `/etc`),
  and `repo` as the hub server, so its tools are `repo__*`.

## Order of work

1. Part 1 (analyst).
2. Part 2: semantic annotations, then repo (read tools), then the hub, then
   the foreman config. Try it with `foreman try` on the sample event, then
   on real 👎s.
3. Part 3: foreman actions, then `open_pull_request`, then `actions:` in the
   task type.

Each step is its own approval and commit.
