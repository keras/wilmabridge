# wilmabridge

A small, read-only CLI that logs into [Wilma](https://wilma.fi/) (Visma InSchool) with a
guardian account, prints messages for your children as
[NDJSON](https://github.com/ndjson/ndjson-spec), and can extract the actionable dated
events buried in them ("koe torstaina 5.3.") using a Gemini model. Fetching (`sync`) and
extraction (`extract`) are stateless pipe stages by default — but `extract` and the new
`ingest`/`review`/`db` commands can optionally persist into a local SQLite file, so the
same Wilma message landing in both kids' inboxes gets deduplicated and extracted once, and
nothing needs to be re-processed on every run. See "Persistence" below.

**Unofficial.** Wilma has no public API for guardians. This talks to the same endpoints
Wilma's own web client uses, reverse-engineered by observing that client (see also the
community's [wilma_api](https://github.com/developerfromjokela/wilma_api) docs, though
they describe an older API version than the one this was built and tested against — the
message list is JSON but the message detail view is server-rendered HTML, scraped with a
regex-based extractor). It can break without notice if Visma changes their backend.

wilmabridge itself never replies, composes, deletes, or explicitly marks anything read —
it only issues GETs plus the login/logout POSTs. **However**: on the Wilma version this
was tested against, simply viewing a message's detail page (which `sync` does whenever
`--bodies=true`, the default) marks it read server-side, the same as it would if you'd
clicked it in a browser. There is no separate metadata-only detail endpoint to peek
without this side effect. If you want a sync run to never change your Wilma read/unread
state, use `--bodies=false` — you'll get subject/sender/timestamp but no body text, and
nothing will be touched.

If your school uses Suomi.fi strong identification for guardian login, password login
will not work — Wilma's built-in "forward messages to email" setting plus your own mail
client is the practical alternative in that case.

## Install / build

```sh
go build -o wilmabridge ./cmd/wilmabridge
```

Requires Go 1.25+. `sync`, `extract` (stdin mode), `roles`, and `probe` use only the
standard library. Persistence (`ingest`, `extract --db`, `review`, `db`) depends on
[`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — a pure-Go SQLite driver, no
cgo, so cross-compiling and shipping a single static binary still works exactly as before
(`CGO_ENABLED=0 go build ./...` is part of this project's own verification). Being pure Go
means it's a real C-to-Go transpile of SQLite rather than a thin wrapper, so it pulls in
~10 supporting modules and noticeably lengthens build time — an accepted tradeoff for
staying cgo-free, not a bug.

## Credentials

Read from the environment only — never from flags, so they never land in shell history
or `ps` output:

```sh
export WILMA_USER=your-wilma-username
export WILMA_PASSWORD=your-wilma-password
export WILMA_HOST=espoo.inschool.fi        # your municipality's Wilma host
export WILMA_CHILDREN='!012345=Aino,!067890=Väinö'   # optional, see below
```

## Usage

```sh
# First time: see what role prefixes Wilma has for your account (one per child).
wilmabridge roles

# Then label them so output is readable:
export WILMA_CHILDREN='!012345=Aino,!067890=Väinö'

# Pull the last week of inbox messages, newest first, as NDJSON:
wilmabridge sync --since 168h

# Incremental / cron-friendly: only messages newer than the last one you saw.
wilmabridge sync --after 48213

# Metadata only, no body fetch (fast, for a quick count/skim):
wilmabridge sync --since 720h --bodies=false | wc -l

# Debugging a new/unfamiliar Wilma instance:
wilmabridge probe
```

`sync` itself is **stateless** by design — no database, no "seen" tracking. Every run you
tell it the window you want via `--since` (a duration before now) and/or `--after` (a
message ID floor); combine them as needed. Bookkeeping across runs (e.g. remembering the
highest ID seen) is left to whatever consumes the NDJSON stream — that can be your own
shell history, or `wilmabridge db last-id` if you're using the SQLite persistence layer
(see below), which tracks it for you.

### `sync` flags

| flag | default | meaning |
|---|---|---|
| `--host` | `$WILMA_HOST` | Wilma host, e.g. `espoo.inschool.fi` |
| `--since` | `168h` | only messages newer than now minus this duration (`0` disables) |
| `--after` | `0` | only messages with `Id` greater than this |
| `--folder` | inbox | `all`, `outbox`, `archive`, `drafts` |
| `--role` | all | restrict to one role prefix (repeatable) |
| `--children` | `$WILMA_CHILDREN` | label map `!id=Name,!id=Name` |
| `--bodies` | `true` | fetch full message bodies (`false` = metadata only, and leaves read/unread state untouched — see caveat above) |
| `--delay` | `300ms` | pause between HTTP requests (politeness) |
| `-v` | off | log each HTTP request to stderr (stdout stays pure NDJSON) |

### Output record

One JSON object per line on stdout:

```json
{"child":"Aino","role":"/!012345","id":48211,"subject":"Retkipäivä 3.9.",
 "sender":"Virtanen Maija","sender_id":9912,"folder":"inbox",
 "sent_at":"2026-08-11T09:14:00+03:00","sent_at_raw":"2026-08-11 09:14",
 "unread":true,"replies":2,"recipients":["8A huoltajat"],
 "body_text":"Hei,\n\nRetki on...","body_html":"<p>Hei,</p>...",
 "url":"https://espoo.inschool.fi/!012345/messages/48211"}
```

`sent_at` is omitted if Wilma's timestamp couldn't be parsed; `sent_at_raw` is always
present so no data is lost. If a message's body fails to fetch, the record still comes
through with a `body_error` field instead of aborting the whole run.

## `extract`: turning message prose into dated events

```sh
wilmabridge sync --since 168h | wilmabridge extract > events.ndjson
```

`extract` reads `sync`'s NDJSON on stdin and, for each message, asks a Gemini model
(Google AI Studio) to pull out actionable dated events — "koe torstaina 5.3.", "vastausaikaa
14.4. asti" — as structured candidates. Go then resolves each candidate's bare date
(`"4.3."`, no year) against the message's send date, and cross-checks any weekday the
message wrote against the resolved calendar date. **It never expands a recurring event
("neljänä perättäisenä viikkona") into multiple dated occurrences** — that's out of scope
for this stage; the recurrence hint is preserved on the output record but only one event is
emitted per candidate.

With no `--db` flag, this is a pure stdin→stdout transform, no file written anywhere — the
mode this was originally built and tuned in, and still the right one for quick iteration on
the prompt (`internal/extract/prompt.go`) or schema from a shell. Add `--db <path>` to pull
from a persistent pending queue instead and save the results — see "Persistence" below.

### Credentials

```sh
export AISTUDIO_KEY=your-google-ai-studio-api-key
```

**Tier matters.** Google's free tier states prompts/responses may be used to improve
Google's products; the paid tier states they are not. This pipeline sends real school
correspondence about named children. Decide deliberately which tier you're on — don't
drift into the free tier by default. (Confirmed live: this project hit the free tier's
`15 requests/minute` cap on a 68-message backfill — if you see `HTTP 429` /
`generate_content_free_tier_requests` in the errors, that's what happened; either raise
`--delay` well above the default for large backfills, or move to the paid tier.)

### Flags

| flag | default | meaning |
|---|---|---|
| `--api-key-env` | `AISTUDIO_KEY` | name of the env var holding the API key |
| `--model` | `gemini-3.5-flash-lite` | Gemini model id |
| `--base-url` | (real endpoint) | override, mainly for tests/a local proxy |
| `--delay` | `200ms` | pause between API calls |
| `--max-retries` | `3` | attempts on HTTP 429/5xx *within one call* before giving up on it; a 4xx other than 429 is never retried |
| `--db` | `$WILMA_DB` | SQLite path; enables persistent pending-queue mode instead of stdin (see "Persistence") |
| `--limit` | `20` | max pending messages to process this run (`--db` mode only) |
| `--max-attempts` | `5` | mark a message `failed` (stop retrying) after this many failed *runs*, not HTTP retries (`--db` mode only) |
| `-v` | off | log the full prompt, latency, token usage, and raw model output to stderr — see below |

Messages with empty `body_text` (i.e. fetched via `sync --bodies=false`) are skipped with
a stderr note — extraction needs the body.

### Debugging / tuning the prompt with `-v`

`-v` prints, per message, exactly what was sent and exactly what came back — this is the
tool for tuning `internal/extract/prompt.go` or `schema.go`:

```
gemini: model=gemini-3.5-flash-lite prompt (626 bytes):
Olet kouluviestien jäsentäjä. Poimi viestistä päivämäärälliset tapahtumat.
...
VIESTI:
Otsikko: Kertaus lukuvuoden viimeisten koulupäivien ohjelmasta
Keskiviikko 27.5. ...
POST https://generativelanguage.googleapis.com/v1beta/interactions
  -> 200 OK (1123 bytes, 2.455s)
  usage: 369 tokens total (218 in / 151 out)
  model_output (359 bytes):
{"candidates":[{"kind":"event","title":"...","date":"27.5.","weekday_claim":"ke", ...}]}
```

The `model_output` block is the model's raw JSON, before Go maps it onto an `Event` and
before any date resolution or validation — so if an event's `resolved_date`/`weekday_ok`
looks wrong on stdout, `-v` tells you whether the model got the raw date wrong, or the model
was right and a Go validator is the one with the bug. Stdout stays pure NDJSON throughout;
all of this goes to stderr, so `sync | extract -v 2>debug.log > events.ndjson` keeps both
streams clean and separable. Very long prompts/outputs are capped at 12000 bytes as a
safety net against a pathological message — real school messages don't come close.

### Output record

One JSON object per **event** (not per message — one message can yield several):

```json
{"wilma_id":19790210,"child":"Jooa Taina","role":"/!04292956",
 "message_subject":"Kertaus lukuvuoden viimeisten koulupäivien ohjelmasta - A recap of the program for the last days of school",
 "message_url":"https://espoo.inschool.fi/!04292956/messages/19790210",
 "kind":"event","title":"Lukuvuoden päättäjäiset","date_raw":"30.5.","weekday_claim":"la",
 "time":"9.00","model_confidence":0.95,
 "quote":"Lauantai 30.5.\n\nLukuvuoden päättäjäiset kello 9.00 alkaen.",
 "resolved_date":"2026-05-30","weekday_ok":true,
 "needs_review":true,"review_reasons":["2026-05-30 falls on a weekend (lauantai)"],
 "extract_ver":1}
```

That's an actual event from a live run — end-of-year ceremony genuinely falls on a
Saturday, so it's correctly flagged for a human glance rather than either silently trusted
or silently dropped. `needs_review`/`review_reasons` is the whole safety mechanism here:
nothing is ever dropped for looking suspicious, and nothing is ever guessed at when a date
can't be parsed — `resolved_date` and `weekday_ok` are simply absent in that case, with the
reason spelled out. Also observed live: real messages are messier than the hand-written
test cases — date ranges (`"1.-7.4"`), ISO week numbers (`"viikolla 20"`), bare weekdays
(`"torstaina"`) and even a genuine weekday/date mismatch in a real school message all show
up as `needs_review` rather than a wrong or crashed result. Expect a real review queue in
practice, not an edge case.

`model_confidence` is the model's self-reported score — **do not trust it**; it reported
`1.0` on the one live response where a schema bug caused it to silently drop data. All
`needs_review` decisions come from the deterministic Go validators, never from this field.

## Persistence: `ingest`, `extract --db`, `review`, `db last-id`

```sh
export WILMA_DB=wilma.db     # or pass --db explicitly on each command below

wilmabridge sync --since 168h | wilmabridge ingest
wilmabridge extract --limit 20
wilmabridge review
```

Why this exists: `sync`'s NDJSON has one line per **(child, message)** pair, so a
school-wide message sent to both kids appears twice with the same Wilma ID — confirmed
live, message `19824275` ("Kouluterveydenhoitajan info") showed up identically in both
inboxes. Without persistence, that means extracting the same message twice. `ingest`
dedupes it: the same Wilma ID + identical content collapses into **one** `messages` row
(extracted once), while a `message_children` row is still recorded per child, so it's known
to concern both.

- **`wilmabridge ingest --db <path>`** — reads `sync`'s NDJSON on stdin, stores each message
  (deduplicated as above), and records each role's highest ingested message ID. A
  body-less message (from `sync --bodies=false`) is stored as `extract_state=skipped`
  immediately rather than sitting in the queue forever, since it can never be extracted.
- **`wilmabridge extract --db <path>`** — instead of stdin, pulls up to `--limit` messages
  with `extract_state=pending`, extracts each, and persists the events, an audit row of the
  exact Gemini request/response (table `extractions` — this is `gemini.RawExchange`, which
  the stdin-mode command computes too but has nowhere to put), and the message's new state.
  A message that keeps failing (quota, malformed output, ...) is retried on each future run
  until `--max-attempts` failed *runs* have accumulated, then marked `failed` and left alone
  — the retry budget is finite, not infinite. **Events still print to stdout in this mode
  too**, so `-v` and piping keep working exactly as in stdin mode.
- **`wilmabridge review --db <path>`** — NDJSON of every event flagged `needs_review`,
  annotated with which children it concerns (joined through `message_children`, since an
  event itself doesn't carry a child — it belongs to the message, extracted once regardless
  of how many kids received it).
- **`wilmabridge db last-id --db <path> [--role <prefix>]`** — prints the stored per-role
  high-water mark, so a cron job can do `sync --after $(wilmabridge db last-id --role !X)`
  without tracking that number anywhere else.

Re-running `ingest` or `extract --db` over data that's already been processed is a safe
no-op — verified live: a second `sync | ingest` pass over the same window reported
`0 new`, and a second `extract --db` pass found nothing pending and made zero new Gemini
calls, changed zero rows.

**What this deliberately doesn't do yet**: no reminders table, no scheduling policy (when
a reminder should fire, which channel it goes to), no delivery. `review` is the whole
"what needs my attention" surface for now. See `openclaw-integration.md` for where
reminders/delivery are headed once those design questions (channel, timing) are settled.

## Exit codes

- `0` — success (including zero matching messages)
- `1` — runtime failure (network, login, or a message that failed to list — check stderr)
- `2` — usage/configuration error (missing `--host`/`$WILMA_HOST`, missing credentials)
