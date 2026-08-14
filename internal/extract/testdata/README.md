# Fixture provenance

Each `*.json` file is `{"request": <exact Interactions API request>, "response": <exact
response>}`, captured live against `gemini-3.5-flash-lite` while developing the extraction
prompt/schema (see `openclaw-integration.md` for the worked-example context).

- `monthly_letter.json` — real capture. Four dated lines, one message, no recurrence.
- `recurrence.json` — real capture, **after** fixing the schema to mark `recurrence`'s
  `freq`/`count` as `required`. Before that fix, the same message caused Gemini to silently
  drop `count` (see the git history / conversation this was built from) — this fixture is
  the corrected, working exchange, not the buggy one. Also the fixture for
  `TestExtractMessage_Recurrence`, which now asserts the ver-3 expansion behavior (one
  candidate → 4 independent Event rows, one per weekly occurrence) rather than the ver-1/2
  behavior of one Event carrying a `{freq,count}` hint — see `schema.go`'s `ExtractVer`
  doc comment.
- `no_date.json` — real capture. Message has no actionable date; Gemini correctly returns
  `candidates: []`. Guards against hallucinated events.
- `deadline.json` — **reconstructed**, not a byte-identical capture: its response file was
  overwritten by a later call in the same debugging loop before it could be saved. The
  `request` is real (same schema as `recurrence.json`, real message text). The `response`
  envelope shape mirrors the other three genuine captures; the `model_output` text is the
  exact text Gemini returned, transcribed from the conversation where it was printed and
  reviewed. Marked with `"_note"` in the file itself. Treat it as slightly lower-confidence
  than the other three — if it ever looks suspicious, re-run the live call and replace it.

These four are the seed of the prompt-regression corpus `openclaw-integration.md` calls for.
Add more real captures here as new message shapes turn up.

All four also predate **ver 3**, which removed `location`/`items` from the schema (folded
into `detail` instead). Their captured `response` text still contains `"location"`/`"items"`
keys in the model's raw JSON — that's expected and harmless: `Candidate` no longer declares
those fields, so `encoding/json` silently ignores them on decode. Nothing needs editing in
the fixture files themselves; they're a record of what the model actually returned under an
older schema, not of what today's code does with it.

All four were captured under the **ver-1** prompt/schema, i.e. before the `audience` field
existed (see `internal/extract/schema.go`'s `ExtractVer` doc comment). Replaying them today
is expected to produce events with `audience` defaulted to `"child"` and flagged
`needs_review` — that's `BuildEvent`'s never-silently-guess default working as designed, not
a regression; `extract_test.go`'s `TestExtractMessage_MonthlyLetter` asserts exactly this.

- `audience_guardians.json` — **synthetic, not a live capture.** Hand-constructed (marked
  `"_note"` in the file) to unblock Go-side tests for the ver-2 `audience` field before a real
  guardians-only message (e.g. a genuine vanhempainilta notice) has been captured live. This
  is a *weaker* provenance class than even `deadline.json`'s reconstruction-from-transcript —
  no live Interactions API call was ever made for it. The schema's own doc comment records a
  real incident (`recurrence.count` silently dropped) where an unverified field turned out not
  to work as expected; treat `audience` the same way until it's replaced with a genuine
  capture and the swap is confirmed not to change the parsed result.
