# garage-ui MCP: spec (specced 2026-10-25)

garage-ui serves its own MCP tools, so an agent can copy, move and delete
files and folders in the buckets, through [mcp-hub](../mcp-hub/SPEC.md) as
`garage-ui__*`. First user: [nexus](../nexus/SPEC.md) ("move
uk/crime/2026-08.zip into archive/", "delete the isos I downloaded
yesterday").

These are the only tools in the repo that destroy data, so each one has a
**preview** that says exactly what it would do, and the card shows it
before you press **Start**.

## The tools

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `transfer` | no | `{"op": "copy"\|"move", "items": [{"src_bucket", "src_key", "dst_bucket", "dst_key"}], "overwrite"?}` → what the page's Copy to…, Move to… and Rename do (`POST /api/transfer`). A long tool: returns `{"run_id", "topic": "garage-ui.transfer"}`. `_meta: {"preview": "check_transfer", "wait": "2h"}`. |
| `check_transfer` | yes | The same arguments → what `transfer` would do, without doing it: the files, their total size, the first 20 `src → dst` pairs, and the destinations that already exist. Fails with the same errors `transfer` would. |
| `delete` | no | `{"bucket", "keys": [...]}` → what the page's Delete does (`POST /api/delete`): files, and folders (keys ending in `/`) with everything in them. A quick tool: returns `{"deleted": n}`. `_meta: {"preview": "check_delete"}`. |
| `check_delete` | yes | The same arguments → what `delete` would remove: the count, total size and the first 20 keys. Fails with the same errors `delete` would. |

- **One path for every request.** `transfer` and `delete` are split into
  *plan* (the checks and the list of objects, as today) and *do*. The page,
  the tools and the previews all call the same plan, so a preview can't
  say something other than what happens.
- **Same rules as the page:** no empty or `/` key, source and destination
  both folders or both files, no folder into itself, nothing overwritten
  without `overwrite`, sources of a move deleted only after every copy
  succeeded.
- **Limits for the tools only.** `delete` refuses more than 1,000 objects
  in one call, and `transfer` more than 1,000 objects or 100 GB: "use
  garage-ui's page for this". A misread request can't empty a bucket. The
  page keeps no limit.
- **`transfer` is long** because a folder of large files can take longer
  than the hub's 60 s timeout. It runs in the background, one at a time
  (a second waits for the first), and publishes `started`, then `ok`
  (`{"count", "bytes"}`) or `failed` (`{"error"}`) on relay topic
  `garage-ui.transfer` with its `run_id`.
  vigil shows it. A transfer cut short by a restart is `failed`
  ("interrupted"), not resumed: a move never deleted its sources, so
  starting it again is safe.
- `delete` stays quick: S3 deletes 1,000 keys per request, so 1,000
  objects is one call.
- **No listing tools.** Browsing is url-to-bucket's `list_objects`,
  already in the `nexus` profile. garage-ui's key may see buckets
  url-to-bucket's doesn't; for those, the model only has the keys you give
  it.
- New folder isn't offered: a copy or move creates the folder it writes
  to.

## Serving

- MCP Streamable HTTP at `/mcp` on the existing listener (`-listen`,
  `[::]:8080`), with the official Go SDK, as url-to-bucket does.
- **Auth:** `Authorization: Bearer $GARAGE_UI_MCP_TOKEN` on `/mcp`. `guard`
  passes `/mcp` to the bearer check instead of basic auth. Without a
  token, `/mcp` isn't served.
- `install.sh` writes `GARAGE_UI_MCP_TOKEN` on the first run, and
  `EnvironmentFile=-/etc/relay-client.env` for publishing. Without relay,
  `transfer` is a tool error ("relay is not configured"); `delete` works.

## Changes outside garage-ui

- **nexus: previews.** nexus doesn't read `_meta.preview` today. It gets
  foreman's rule ([foreman actions](../foreman/SPEC-actions.md),
  "Preview"): when a call is proposed, nexus calls the preview tool with
  the same arguments; if it fails, the error goes back to the model and no
  card is made; if it succeeds, its text is stored on the card and shown
  above **Start**. A preview tool that isn't read-only refuses the
  proposal. About 40 lines in `chat.go` and `index.html`, and two tests.
- `mcp-hub/servers.json`: a `garage-ui` server at
  `http://127.0.0.1:8080/mcp`; the `nexus` profile gets the four tools.
- `mcp-hub/install.sh`: `garage-ui` added to the apps whose tokens it
  copies.
- `nexus/config/prompt.md`: when to use each, and to say what will be
  deleted or moved in the reply that proposes it.

## Code

- `mcp.go` (new): the server, the tools, the previews, the background
  transfer and its events. About 200 lines.
- `main.go`: `transfer` and `delete` split into plan and do; `guard`
  routes `/mcp`.
- `install.sh`, `README.md`.

New dependencies: the MCP Go SDK and `relay/client`.

## Tests

With a fake S3 and a fake relay (garage-ui has no tests today; these are
its first):

- `check_delete` and `check_transfer` list exactly what `delete` and
  `transfer` then do.
- The page's errors are tool errors: empty key, folder into itself, an
  existing destination without `overwrite`.
- Over 1,000 objects is refused by the tools, not by the page.
- `transfer` publishes `started` and `ok` with its `run_id`; a failed copy
  publishes `failed`, and a move's sources are still there.
- `/mcp` needs the bearer token; basic auth doesn't open it.
