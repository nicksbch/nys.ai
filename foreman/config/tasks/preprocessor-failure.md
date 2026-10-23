---
description: Explain a failed preprocessor unzip and propose a fix
on_event: preprocessor.* failed # unzip (one archive) and poll (a job can't list its bucket)
agent: oncall
group_by: topic data.job        # one open task per job; poll failures share one
output: explanation fix
---
A preprocessor job failed. It unzips archives found in a garage bucket and
writes their files back to garage, where duckpipe ingests them. Its relay
event is in the first message: data.error holds the error text.

- preprocessor.unzip: one archive failed. data.job is the job, data.key the
  archive, data.to where its files go.
- preprocessor.poll: a job (data.name) could not list its bucket, or the
  state file could not be read. Nothing is unzipped until it recovers.

A failed archive is not tried again by itself: it is retried with
"preprocessor retry" on the server.

1. Read the error. It usually says what went wrong: a corrupt or truncated
   archive, a missing bucket or permission, a full disk.
2. Use vigil's tools to check whether this job failed before (list_events
   with app preprocessor, status failed and since 168h, the last 7 days),
   and whether anything else on the server is wrong right now (list_alerts).
   If it failed before with the same error, say so and when it started.
3. Submit:
   - explanation: what failed and why, in a few plain sentences.
   - fix: the change that would fix it, as concretely as you can (a command,
     a config line, a new upload of the archive), and how to check that it
     worked, including the "preprocessor retry" to run once it is fixed.
