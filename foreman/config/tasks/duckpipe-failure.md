---
description: Explain a failed duckpipe ingestion and propose a fix
on_event: duckpipe.* failed     # relay topic filter, then event types
agent: oncall
group_by: topic data.pipe       # one open task per pipe, not one per failure
output: explanation fix
---
A duckpipe ingestion failed. Its relay event is in the first message:
data.error holds the error text, data.pipe the pipe, data.name the files
and the target table.

1. Read the error. It usually says what went wrong.
2. Use vigil's tools to check whether this pipe failed before
   (list_events with app duckpipe, status failed and since 168h, the last
   7 days), and whether anything else on the server is wrong right now
   (list_alerts). If it failed before with the same error, say so and when
   it started.
3. Submit:
   - explanation: what failed and why, in a few plain sentences.
   - fix: the change that would fix it, as concretely as you can (a command,
     a config line, a change to the file), and how to check that it worked.
