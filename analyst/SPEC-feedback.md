# analyst feedback: spec (draft, not approved)

Part 1 of "Feedback that improves the semantic layer" in
[TODO.md](../TODO.md). A **good / bad** button on each answer. The rating is
saved with the conversation and published to [relay](../relay) as an
`analyst.feedback` event. Part 2, a foreman task that reads bad ratings and
proposes a change to the semantic layer, is not in this spec.

## What the user sees

- Under each finished answer: **👍** and **👎**.
- **👍** saves the rating straight away.
- **👎** saves the rating and opens a one-line box, "What was wrong?
  (optional)", with **Send**. Sending saves the comment with the rating.
- The chosen button stays highlighted. Clicking it again takes the rating
  back. Clicking the other button changes the rating.
- An answer that is still being written has no buttons.
- **Edit** on a question drops that question's rating and the ratings of
  every question after it, as it already drops their answers.

An "answer" is everything the model does for one question: its text, and
the queries it runs, up to the next question. A rating names the question
by its index (from 0), the same index **Edit** uses.

## Storage

Ratings go in the conversation's JSON file, in a new field. Conversations
saved before this change load with no ratings.

```go
type Conversation struct {
	// ... existing fields
	Ratings []Rating `json:"ratings,omitempty"`
}

type Rating struct {
	Question int       `json:"question"` // index of the question, from 0
	Value    string    `json:"value"`    // "good" or "bad"
	Comment  string    `json:"comment,omitempty"`
	Time     time.Time `json:"time"`
}
```

There is at most one rating per question. `rewind(n)` also drops the ratings
whose `Question >= n`.

## API

| Call | What it does |
|------|--------------|
| `PUT /api/conversations/{id}/ratings/{n}` | Body `{"value":"good"\|"bad","comment":"…"}`. Sets the rating of question `n`, replacing any earlier rating, and publishes the event. |
| `DELETE /api/conversations/{id}/ratings/{n}` | Takes the rating back. Nothing is published. |

`GET /api/conversations/{id}` already returns the whole conversation, so it
now includes `ratings`.

Errors:

- `400` if `value` is not `good` or `bad`, if the comment is longer than
  2000 characters, or if there is no question `n`.
- `409` if the conversation is answering a question. This uses the existing
  busy flag, so nothing is rated mid-answer.

## The relay event

The topic is `analyst.feedback`, and the type is `good` or `bad`. Both
values are published. Part 2's foreman task only takes the bad ones
(`on_event: analyst.feedback bad`), so analyst doesn't need to know what
the ratings are used for.

```json
{
  "topic": "analyst.feedback",
  "type": "bad",
  "run_id": "<conversation id>",
  "key": "<conversation id>/<n>/<value>/<sha256 of comment, first 8 hex>",
  "data": {
    "conversation": "c_…",
    "title": "Revenue by region last quarter",
    "model": "local",
    "question_index": 2,
    "question": "and by product line?",
    "answer": "…the model's text for that question…",
    "calls": [
      {"tool": "semantic__query_model", "args": {…}, "error": "unknown dimension 'product_line'"}
    ],
    "comment": "it used the wrong revenue measure"
  }
}
```

- **The event holds what the agent needs** (the question, the answer, and
  the tool calls with their arguments and errors), so part 2 doesn't need a
  way to read analyst's conversations. This answers the open question in
  TODO.md. The event also stays useful after the conversation is edited or
  deleted.
- **Tool results are left out.** Rows can be large, and the arguments and
  errors are what show a gap in the semantic layer. An error's text is cut
  to 500 characters.
- **Size:** relay takes up to 64 KB of `data`. The answer is cut to 16 KB,
  each call's arguments to 2 KB, and only the first 20 calls are kept. A cut
  field ends with `…(cut)`.
- **Idempotency:** with the `key` above, clicking the same rating with the
  same comment again within 24 h doesn't publish a second event.
- **Publishing never blocks the rating.** It uses `client.Go` (it runs in
  the background). If relay isn't configured or is down, the rating is
  still saved, and the error is only logged.

## Code changes

| File | Change |
|------|--------|
| `store.go` | `Rating`, the `Ratings` field |
| `chat.go` | `rewind` drops ratings. A helper, `turnOf(n)`, returns the messages of question `n` |
| `main.go` | The two routes. Publishing through `relay.FromEnv()`, logged once at start if relay isn't configured, as foreman does |
| `feedback.go` | Builds the event data from a conversation and a question index, including the cuts |
| `index.html` | The buttons, the comment box, and the highlighted state from `ratings` |
| `go.mod` | `require relay v0.0.0`, `replace relay => ../relay`, as foreman has |
| `install.sh` | `EnvironmentFile=-/etc/relay-client.env` in the unit |
| `README.md` | A **Feedback** bullet, the event, and the routes |
| `analyst_test.go` | Tests listed below |

## Tests

- Rating a question, re-rating it, and taking the rating back. The file on
  disk matches each time.
- Rating question `n` when there is no question `n` returns `400`. Rating
  while the conversation is busy returns `409`.
- `rewind(n)` keeps the ratings before `n` and drops the rest.
- The event data for a turn with text, a failed call and a successful call
  has the right question, answer, calls and error, and no results.
- A huge answer and many calls give data under 64 KB, with the cuts marked.
- With a test relay server, a rating publishes one event with the right
  topic, type, `run_id` and key. Without `RELAY_URL`, the rating still
  saves.

## Not in this spec

- The foreman task (part 2) and opening PRs (part 3).
- A page that lists all the feedback. relay's `/ui` and
  `relay tail -topic analyst.feedback` show it for now.
- Rating reports.
