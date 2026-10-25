# atlas: what is running now (spec, approved 2026-10-25)

A **Running** tab in atlas that shows everything going on across the
system at that moment: downloads, unzips, duckpipe loads, lakeshape runs
and foreman tasks, each with what it is working on and how long it has
been going. It also shows what is waiting to start and what just ended.
The running nodes are marked on the graph as well.

Today, to see this you open url-to-bucket, preprocessor, duckpipe and
lakeshape one by one.

## Why it can be done from relay alone

Every service that does work already publishes it in the same shape: a
`started` event and then `ok` or `failed`, with the same run ID.

| Topic | Who | `started` carries |
|-------|-----|-------------------|
| `url-to-bucket.download` | url-to-bucket | url, bucket, key |
| `preprocessor.unzip` | preprocessor | job, key, to, bytes |
| `duckpipe.ingest` | duckpipe | pipe, table, files, bytes |
| `lakeshape.run` | lakeshape | jobs, models, target |
| foreman task topics | foreman | task, agent |

So **running** means a `started` with no `ok` or `failed` for its run ID.
atlas already reads relay, so it doesn't need to call any service, use any
service's password or learn any service's API. atlas finds runs by the
event shape, not by topic, so a new service that follows the convention
shows up without changes to atlas.

## The page

A third tab, **Running**, next to Graph and Configs, in three parts:

- **Running:** one row per run: the service, its `name` (for example
  `uk-crime: 3 files from s3://home/uk/crime/extracted/ … -> raw.crime`),
  when it started and how long it has been going, and a link to the
  service's page. A lakeshape run also shows its progress as models
  finished out of models selected, counted from its `lakeshape.model`
  events with the same run ID.
- **Waiting:** a `requested` with no `started` for its run ID yet, for
  example a download asked for from nexus, or a lakeshape trigger.
- **Just ended:** runs that ended in the last 15 minutes, `ok` or
  `failed` (failed in red, with its error), so a run that finishes between
  two looks at the page isn't lost.

On the **Graph** tab, a node with a run going gets a "running" mark, and
clicking the node lists its runs. A run belongs to a node by the same
matching atlas already uses for each node's last event.

The tab refreshes every 5 s while it is visible, the graph every 30 s as
it does now. The data comes from a new `GET /api/running`, which reads
only relay (no config files), so polling it often is cheap.

## How it reads relay

- `GET /v1/events?type=started,requested&last=500`: the candidates.
- `GET /v1/events?type=ok,failed&last=1000`: what has ended.
- A candidate whose result isn't in that window is looked up by its run
  ID (`run_id=…`), so a long lakeshape run whose `started` has scrolled
  out of the newest 1000 events isn't shown as running by mistake.

## Runs that never end

A service killed in the middle of a run never publishes the result, so
its `started` stays unanswered. atlas can't tell that apart from a run
that is just slow. It shows the age, and a run going for more than 6
hours is moved from Running to a short **No result** list, with
"started 9 h ago, no result". atlas doesn't hide these runs, because a
lost run is worth seeing. Runs that started more than 24 hours ago are
left out, so they aren't looked up again on every refresh.

## Not in this

- **Files waiting in a queue** (archives preprocessor hasn't reached,
  files duckpipe hasn't loaded) aren't relay events. Showing them would
  mean calling each service's `/api/state` with its password. That is a
  separate decision. The service's own page still shows them.
- **Byte-level progress** for a download or an unzip: no event carries
  it. Only elapsed time is shown.
- **Actions** (stop, retry). atlas stays read-only and links to the
  service's page.

## Changes

| File | Change |
|------|--------|
| `atlas/relay.go` | `run_id` on events; fetch candidates, results, look-ups by run ID |
| `atlas/running.go` (new) | pair events into running / waiting / ended / no result, and place runs on graph nodes |
| `atlas/server.go` | `GET /api/running` |
| `atlas/ui.html` | the Running tab, the mark on graph nodes |
| `atlas/running_test.go` (new) | pairing, the 6 h rule, the look-up by run ID |
| `atlas/README.md` | the tab |

No changes to any other service.
