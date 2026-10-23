---
description: Explain a failed lakeshape build and propose a fix
on_event: lakeshape.* failed    # model (one model), run (a whole run), project (the project doesn't load)
agent: oncall
group_by: type                  # one open task for lakeshape: a broken run fails several ways at once
output: explanation fix
---
A lakeshape build failed. lakeshape builds the analytics models (SQL) in
the DuckLake lakehouse from the tables duckpipe loads. Its relay event is in
the first message: data.error holds the error text.

- lakeshape.model: one model failed. data.model is the model, data.relation
  the table it builds, data.tests the tests that failed, if any.
- lakeshape.run: a run failed. data.jobs are its jobs. data.error lists the
  models that failed, or says why the run couldn't start.
- lakeshape.project: the project doesn't load (a bad model file or job),
  so nothing runs until it is fixed.

Later failures are attached to this task, so the first event may be one of
several from the same run.

1. Read the error. A model error is usually a SQL error: a missing column
   or table, a type that doesn't convert, a failed test.
2. Use vigil's tools: get_run with the event's run_id gives every model of
   that run, with the full errors. list_events with app lakeshape, status
   failed and since 168h shows whether it failed before. Also check
   list_events with app duckpipe for failed loads: a model often fails
   because the table it reads from didn't load or changed shape.
3. Submit:
   - explanation: what failed and why, in a few plain sentences. Name the
     model that failed first; models downstream of it are skipped, not
     broken.
   - fix: the change that would fix it, as concretely as you can (the SQL
     or config change, or the upstream load to fix first), and how to check
     that it worked ("lakeshape trigger <job>" on the server runs it again).
