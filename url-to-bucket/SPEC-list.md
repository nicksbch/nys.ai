# url-to-bucket list: spec (not approved yet)

A durable list of files in url-to-bucket: each URL and its S3 destination.
You add URLs, in bulk if you like, and the service downloads whatever isn't
there yet. The list also keeps the history of everything downloaded, from
any source, for good.

Today the only record is relay's: 30 days, and the page reads just the last
500 events. Nothing stops the same file being downloaded twice, and a bulk
download means one `download` call per URL.

## What it does

- **Add:** a list of `{url, bucket, key?}`. `key` works as it does today
  (full key, folder ending in `/`, or empty) and is resolved when added.
  Adding the same thing twice changes nothing.
- **Sync:** the service works through the entries by itself. There is no
  sync command.
- **History:** every download is recorded with its destination, size, time
  and outcome, including those started from the page, `download`,
  `relay pub`, nexus, foreman or the CLI.

## The list

One entry per destination (`bucket` + resolved `key`):

| Field | Meaning |
|-------|---------|
| `url`, `bucket`, `key` | Where it comes from and where it goes |
| `state` | `pending`, `queued`, `running`, `ok`, `failed`, or `present` |
| `added` | When it was added; empty if it came from history only |
| `bytes`, `error`, `finished` | From the latest run |
| `runs` | Every run: `run_id`, outcome, bytes, duration, time |

`present` means the object was already in the bucket and was not downloaded.

Stored as one JSON file, `/var/lib/url-to-bucket/list.json`, replaced
atomically on each change, like preprocessor's `state.json`. No new
dependency. The systemd unit gains `StateDirectory=url-to-bucket`.

## Rules

- **A destination holds one URL.** Adding a different URL for a destination
  already listed is refused for that item; the rest of the batch still goes
  in. This matters for bulk: `…/download?id=1` and `…/download?id=2` both
  resolve to the key `download`, and today the second would silently replace
  the first. The same URL may go to two destinations.
- **Never replaces by accident.** Before downloading an entry, the service
  sends a HEAD to the destination. If an object is there, the entry becomes
  `present`, with the object's size and date. This covers files downloaded
  before the list existed.
- **Replacing on purpose** stays with the existing `download` tool, which
  overwrites as it does today. The list records it as a new run.
- **Done means done.** An `ok` entry is not fetched again, and nothing
  checks whether the bucket or the remote file changed since.

## How sync runs

The list feeds the existing path and doesn't replace it:

1. A loop publishes the normal `requested` event for the next `pending`
   entries, with at most `-workers` queued or running at a time. Downloads
   then go through the existing workers, so `started`/`ok`/`failed`, vigil,
   the page and restart recovery all work unchanged.
2. A new relay consumer, `url-to-bucket-list` (all four event types on the
   topic, starting from `earliest`), folds events into the list. Replayed
   events change nothing, because each run is stored by `run_id` and event
   ID. Reading relay rather than hooking `runJob` is what catches CLI runs
   from another machine. Starting from `earliest` also fills the list with
   whatever relay still holds, about 30 days.

Why the requests are paced instead of all published at once:

- The page reads the last 500 events, so 500 queued requests would push the
  running downloads off it.
- Entries not yet published can still be removed.

**Failures stop the batch.** After 5 failures in a row with no success
between them, the loop pauses and the rest stay `pending`. Each failed URL
opens its own foreman task (`group_by: topic data.url`), so a dead host or a
full disk across a 500-URL batch would otherwise start 500 on-call agents.
There is no automatic retry. `retry_failed` queues the failed entries again
and lifts the pause.

## Interfaces

MCP, at `/mcp`:

| Tool | Read-only | What it does |
|------|-----------|--------------|
| `add_files` | no | `{files: [{url, bucket, key?}]}`, at most 1000. Returns each item as `added`, `already_listed` or `refused`, with the reason and the resolved key |
| `list_files` | yes | `{state?, bucket?, prefix?, url?, limit?}`: entries newest first, with a count per state and whether sync is paused. 50 by default, at most 500 |
| `retry_failed` | no | `{bucket?, prefix?}`: puts failed entries back to `pending` and lifts the pause |
| `remove_file` | no | `{bucket, key}`: `pending` or `failed` entries only, so the history stays honest. Never touches the bucket |

**Hub:** nexus's profile gets all four, with the usual **Start** card on the
three that change things. `oncall` gets none.

**Page:** a new **List** card. A text box takes one URL per line, with a
bucket and a folder, which covers the common bulk case. Below it, a table
filtered by state, with the paused banner and **Retry failed**. The API
behind it is `POST /api/files`, `GET /api/files` and
`POST /api/files/retry`, under the page's existing login and header check.

No new CLI commands: MCP and the HTTP API cover agents and scripts.

## Not in this version

- Finding URLs on a page (for example, each month's file on data.police.uk).
- Scheduled re-downloads, or noticing a remote file has changed.
- Checking that the bucket still matches the list, or checksums.
- Loading the list into the lake.

## Code

- `list.go` (new): the store, the sync loop and the event fold.
- `mcp.go`, `ui.go`, `ui.html`: the tools, API and card above.
- `serve.go`: start the loop and the consumer.
- `install.sh`: `StateDirectory`.
- `mcp-hub/servers.json`: nexus's `allow` list.
- `README.md`.

## Tests

- Add: repeating a batch changes nothing; a different URL for a listed
  destination is refused alone; a folder key resolves per URL.
- Sync: never more than `-workers` in flight; an existing object becomes
  `present` with no request published; 5 failures in a row pause it, and
  `retry_failed` resumes it.
- Fold: replayed events change nothing; a run started elsewhere creates an
  entry with no `added`; an invalid request's `failed`, which has no URL, is
  ignored.
- Restart: the list and the pause survive it, and a `queued` entry isn't
  published again.
- Remove: refused for `ok`, `present`, `queued` and `running`.
