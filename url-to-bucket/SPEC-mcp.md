# url-to-bucket MCP: spec (approved 2026-10-25)

url-to-bucket serves its own MCP tool, `download`, so an agent can ask for
a download through [mcp-hub](../mcp-hub/SPEC.md) as
`url-to-bucket__download`. It is the first action tool for
[foreman actions](../foreman/SPEC-actions.md): the on-call agent for a
failed download can propose downloading it again, and it runs after you
approve it.

## The tool

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `download` | **no** | `{"url", "bucket", "key"?, "content_type"?}` → publishes a `requested` event, exactly as the web page does, and returns `{"run_id", "topic": "url-to-bucket.download"}` at once. |

- **One path for every request.** The tool calls the same code as the
  page's request (`ui.request`): the same checks (`requestEvent.job`: an
  http or https URL, a bucket), then the same `requested` event on relay.
  The download then runs as any other: `started`, then `ok` or `failed`,
  on `url-to-bucket.download` with that `run_id`. That is what foreman
  follows.
- A request that fails the checks is a tool error (`isError: true`) with
  the same text as on the page, and nothing is published.
- `_meta: {"wait": "2h"}`, for large files.
- The tool's description says what it does and when not to use it: "Don't
  retry a URL that failed with a 4xx; it will fail the same way."
- **`list_buckets`** (read-only, added 2026-10-25 for
  [nexus](../nexus/SPEC.md)): `{}` → `{"buckets": [...]}`, the buckets the
  service's key can see, as the page's bucket list shows. A model then
  doesn't have to guess bucket names.
- **`list_objects`** (read-only, added 2026-10-25 for nexus):
  `{"bucket", "prefix"?}` → one level of the bucket, like `ls`: the
  folders and the files (key, size, last modified) directly under the
  prefix, at most 200, with `truncated` when there are more. A prefix
  without a trailing `/` is taken as a folder. A model uses it to find
  where similar files already are before choosing a key.
- **`suggest_key`** (read-only, added 2026-10-25 for nexus): `{"bucket",
  "url"}` → where the file belongs. It reads the bucket's keys (at most
  20,000) and finds the folders holding files named like the URL's file:
  the same name with every digit as `#` (`2023-08.zip` is `####-##.zip`),
  or failing that the same extension. Up to 5 suggestions, most matches
  first, each with the full key to download to, up to 3 examples, and
  whether a file is already at that key. None means nothing similar is
  there. The walking that `list_objects` leaves to the model was too much
  for the small local model, which stopped one level down; this is plain
  matching, so the same URL always gets the same answer.
- **`list_downloads`** (read-only, added 2026-10-25 for nexus):
  `{"limit"?}` → `{"downloads": [...]}`, the page's list (`GET
  /api/downloads`), newest first: run id, state (`queued`, `running`,
  `ok`, `failed`), URL, bucket, key, bytes, total and rate while running,
  and the error. 20 by default, at most 100. So "what's downloading?" has
  an answer outside a nexus card.
- **`download`'s description** spells out the key: a full key, or a
  folder ending with `/` (the file keeps its name), never the file's name
  followed by `/`.

## Serving

- MCP Streamable HTTP at `/mcp` on the existing listener (`-listen`,
  `127.0.0.1:8093`), with the official Go SDK, as vigil does (`vigil/mcp.go`).
- **Auth:** `Authorization: Bearer $URL_TO_BUCKET_MCP_TOKEN` on `/mcp`.
  `guard` passes `/mcp` to the bearer check instead of basic auth and the
  `X-Url-To-Bucket` header, which are for the browser.
- `install.sh` writes `URL_TO_BUCKET_MCP_TOKEN` into its env file on the
  first run, as vigil does for `VIGIL_MCP_TOKEN`, and mcp-hub's install
  copies it.

## Hub and foreman config

- `mcp-hub/servers.json`: a `url-to-bucket` server, and the `oncall`
  profile gets `url-to-bucket__download` in its `allow` list.
- `foreman/config/tasks/url-to-bucket-failure.md`: `actions:
  url-to-bucket__download`, and a step in the instructions: "If the failure
  looks transient (a timeout, a 5xx, this server's disk was full and isn't
  now), also call url-to-bucket__download with the same url, bucket and
  key."

## Code

- `mcp.go` (new): the server, the tool, the bearer check. About 80 lines.
- `ui.go`: the request code split so the page and the tool share it;
  `guard` routes `/mcp`.
- `install.sh`, `README.md`.

New dependency: the MCP Go SDK (vigil already uses it).

## Tests

- `download` publishes one `requested` event with the arguments and returns
  its `run_id` and topic.
- A bad URL or a missing bucket is a tool error, and nothing is published.
- `/mcp` needs the bearer token, and the page's basic auth doesn't open it.
