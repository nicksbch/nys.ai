# nexus: spec (approved 2026-10-25)

A chat page for asking the server to do things. You type "get
https://…/debian.iso into isos", a model works out the call, the page shows
it as a card, and the download starts when you press **Start**. The card
then follows the download live until it ends. It is one Go binary with a
built-in page, like analyst.

The first, and for now only, thing it can do is start downloads, through
url-to-bucket's `download` tool. What it can do is exactly what its mcp-hub
profile holds, so a later capability is a tool added to that profile, not
a change to nexus.

## Why not analyst, foreman or Claude Code

- **analyst** answers questions about the lake through the semantic layer
  only, and is built around that (query cards, charts, reports). Giving it
  tools that change things would give it a second job.
- **foreman** runs agents on events, without you there; you only review.
  A chat is the other way round: you ask, and you're there to confirm.
- **Claude Code or Desktop, connected to mcp-hub**, would work with no new
  code. You chose a page of its own: it works from any browser on the LAN,
  uses the local model, and shows each download's progress.

## How a turn works

The same loop as analyst (`chat.go`): the model, the tools it asks for,
their results back, until it answers in text, within `-max-steps` and
`-timeout`.

- **Tools:** at the start of each turn, nexus connects to
  `<hub>/mcp/nexus` and lists the profile's tools.
  - **Read-only tools** (`readOnlyHint: true`) are called right away, as in
    analyst.
  - **Other tools** are only proposed: the call becomes a card on the page
    and the model gets "Proposed. The person starts it on the page, or
    not. Say what you proposed, and don't call it again." Nothing runs
    until you press **Start**. This is foreman's rule, "read-only, or
    approved by you", with you approving in the chat.
- **Before proposing**, the arguments are checked against the tool's input
  schema, as foreman does. A bad argument goes back to the model to
  correct, and never reaches a card.
- A turn can propose several calls ("get these three URLs"): each is its
  own card.
- **Corrections by reply** (added 2026-10-25): you reply with another
  folder or key, and the model proposes again. A new proposal replaces the
  conversation's earlier cards that haven't run (`proposed` or `failed`)
  for the same tool and the same URLs in the arguments; they become
  `superseded` and can't be started. Cards with no URL argument are never
  replaced.

## The card

| State | Meaning |
|-------|---------|
| `proposed` | the tool, its arguments; **Start** and **Cancel** |
| `calling`  | you pressed **Start**; the tool is being called through the hub |
| `running`  | a long tool returned `{"run_id","topic"}`; the card follows the job on relay |
| `done`     | the call or the job succeeded; its result (for a download: `s3://bucket/key`, size, time) |
| `failed`   | the call or the job failed, with the error; **Start again** |
| `cancelled`| you pressed **Cancel** |
| `superseded` | a later proposal for the same URL replaced it (added 2026-10-25) |

- **Start** calls the tool through the hub, once (the state moves to
  `calling` under a lock first), with `X-Run-Id` set to the conversation's
  id, so mcp-hub's audit links the call to it.
- **Following a job:** while a card is `running`, the server reads relay
  for its `run_id` and topic (`GET /v1/events?topic=…&run_id=…`,
  long-polling) and updates the card: `started`, then `ok` or `failed`. A
  download in progress shows its bytes from its `started` event, and links
  to url-to-bucket's page for the live counter. There is no deadline: the
  card stays `running` until the job reports.
- **The model is told the outcome.** When a card ends, a line is added to
  the conversation for the next turn: "The person started url-to-bucket
  download (run abc): ok, s3://isos/debian.iso, 650 MB". So "did it
  finish?" or "now get the next one" work as follow-ups.

## Conversations

Kept as one JSON file each, listed in a sidebar, continued where they left
off, as in analyst. The cards and their states are part of the
conversation. No reports, no editing of questions: analyst's features for
analysis aren't needed here.

## Models and prompt

`config/models.json` and `config/prompt.md`, in analyst's format (chat
completions API; llama-server by default, Claude through Anthropic's
OpenAI-compatible endpoint if you add it). The prompt says what the person
can ask for, that calls are proposed and confirmed, and to ask when the
bucket isn't clear rather than guess.

## Changes outside nexus

- **mcp-hub `servers.json`:** a profile `nexus` with url-to-bucket's
  `download` and `list_buckets`.
- **url-to-bucket:** a read-only tool `suggest_key` (added 2026-10-25):
  the folders already holding files named like the URL's file, with the
  full key to use. The small local model stopped one level into
  `list_objects`, so the matching is done by url-to-bucket instead.
- **url-to-bucket:** a read-only tool `list_objects` (added 2026-10-25),
  one level of a bucket like `ls`, so the model can find where similar
  files already are before choosing a key: without it, a download with no
  path given went to the top of the bucket.
- **url-to-bucket:** a read-only tool `list_buckets`, the buckets the
  service can write to (what the page's bucket list shows, from
  `GET /api/buckets`). Without it the model has to guess bucket names. It
  is a small addition to [SPEC-mcp.md](../url-to-bucket/SPEC-mcp.md).
- **deploy-all.sh** and **home:** an `nexus` step after `mcp-hub`, and a
  card (`nexus=8102`).

## Running

- `nexus serve [-listen 127.0.0.1:8102] [-config /etc/nexus] [-hub http://127.0.0.1:8094] [-data ./data]`
- `nexus check`: the config loads, the hub profile answers, and the
  model takes one tool call (exit codes 0, 1, 2 as in analyst).
- Basic auth (`NEXUS_AUTH`), written on the first install in
  `/etc/nexus.env` with `MCP_HUB_TOKEN` copied from `/etc/mcp-hub.env`.
  `EnvironmentFile=-/etc/relay-client.env` for reading relay.
- systemd with `DynamicUser=yes`, state in `/var/lib/nexus`, on
  `[::]:8102`. Config in the repo in `config/`, installed and checked on
  every deploy, as for analyst.

## Code

About 900–1,100 lines of Go and 500 lines of HTML and JavaScript, much of it
following analyst's files (the chat loop, the model API, the store):

- `main.go`: serve, check, the JSON API, the login.
- `chat.go`: a turn; read-only calls; proposals.
- `model.go`: the chat completions API, `models.json`, `prompt.md`.
- `tools.go`: the hub profile, the schema check, **Start**, the result.
- `jobs.go`: following a `running` card on relay.
- `store.go`: conversations.
- `index.html`: the page.

Dependencies: the MCP Go SDK (with its JSON Schema package) and
`relay/client`. Code is copied from analyst rather than shared: each app in
this repo is its own module.

## Tests

With a fake hub, a fake model and a fake relay:

- A read-only tool is called during the turn; a tool that isn't is not
  called, and becomes a `proposed` card.
- Bad arguments go back to the model and make no card.
- **Start** calls once (a fake hub counts), with the conversation's id;
  two clicks call once; **Cancel** calls nothing; a failed call can be
  started again.
- A long tool's card follows relay: `started`, then `ok` → `done`, or
  `failed` → `failed`; the outcome is in the next turn's messages.
- A restart keeps the conversations and their cards; a card left `calling`
  comes back `failed` ("interrupted"), and one left `running` is followed
  again.
- The API needs the login.

## Not in this spec

- Tools beyond downloads. Each one is a tool added to the `nexus`
  profile, decided when it comes up.
- Starting without confirming, for any tool.
- More than one user, or roles.
- Streaming the model's text token by token. Each step appears when it
  completes, as in analyst.

## Decisions (2026-10-25)

1. **Name:** `nexus`.
2. **Port:** 8102, after atlas (8101).
3. **Confirm every call that changes something.** A misread "get the
   Debian one" never starts a download by itself.
4. **`list_buckets` in url-to-bucket.**
