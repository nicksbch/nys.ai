---
description: Explain a failed url-to-bucket download and propose a fix
on_event: url-to-bucket.download failed
agent: oncall
group_by: topic data.url        # one open task per URL; invalid requests share one
output: explanation fix
actions: url-to-bucket__download  # the agent proposes it; it is called after your approval
---
A url-to-bucket download failed. Its relay event is in the first message:
data.error holds the error text, data.url the URL, and data.bucket and
data.key where it was going. An event without data.url is a request that
was refused before downloading (a bad URL, an unknown bucket).

1. Read the error. It usually says what went wrong: an HTTP status from the
   remote server, a timeout, a refused write to garage.
2. Use vigil's tools to check whether downloads failed before (list_events
   with app url-to-bucket, status failed and since 168h, the last 7 days),
   and whether anything else on the server is wrong right now (list_alerts,
   for example a full disk). If other URLs fail the same way, say so: the
   cause is then on this side, not the remote one.
3. If the failure looks transient (a timeout, a 5xx from the remote server,
   this server's disk was full and isn't now), call url-to-bucket__download
   with the same url, bucket and key. It doesn't download anything now: a
   person approves it first. Don't call it for a 4xx or a refused request;
   it would fail the same way.
4. Submit:
   - explanation: what failed and why, in a few plain sentences. Say
     whether the cause is the remote server, the request, or this server.
   - fix: the change that would fix it, as concretely as you can (retry
     later, a corrected request, a command on the server), and how to check
     that it worked.
