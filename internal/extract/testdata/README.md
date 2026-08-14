# Fixture provenance

Each `*.json` file is `{"request": <exact Interactions API request>, "response": <exact
response>}`, captured live against `gemini-3.5-flash-lite` while developing the extraction
prompt/schema (see `openclaw-integration.md` for the worked-example context).

- `monthly_letter.json` — real capture. Four dated lines, one message, no recurrence.
- `recurrence.json` — real capture, **after** fixing the schema to mark `recurrence`'s
  `freq`/`count` as `required`. Before that fix, the same message caused Gemini to silently
  drop `count` (see the git history / conversation this was built from) — this fixture is
  the corrected, working exchange, not the buggy one.
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
