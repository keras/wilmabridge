# wilmabridge

A small, read-only CLI that logs into [Wilma](https://wilma.fi/) (Visma InSchool) with a
guardian account, prints messages for your children as
[NDJSON](https://github.com/ndjson/ndjson-spec), and can extract the actionable dated
events buried in them ("koe torstaina 5.3.") using a Gemini model. Fetching (`sync`) and
extraction (`extract`) are stateless pipe stages by default — but `extract` and the
`ingest`/`review`/`db`/`poll`/`reextract` commands can optionally persist into a local
SQLite file, so the same Wilma message landing in both kids' inboxes gets deduplicated and
extracted once, and nothing needs to be re-processed on every run. See "Persistence" and
"poll" below.

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

This applies to `poll` too, and there it's easy to trigger by accident: a first `poll` on
an empty database fetches bodies for every message in its `--bootstrap` window (30 days by
default), marking up to a month of history read in one go. Use `--from-now` if you'd
rather start clean from the newest message, or `--bodies=false` for the fully non-invasive
mode (see the caveat about that under "poll" below).

If your school uses Suomi.fi strong identification for guardian login, password login
will not work — Wilma's built-in "forward messages to email" setting plus your own mail
client is the practical alternative in that case.

## Install / build

```sh
go build -o wilmabridge ./cmd/wilmabridge
```

Requires Go 1.25+. `sync`, `extract` (stdin mode), `roles`, and `probe` use only the
standard library. Persistence (`ingest`, `extract --db`, `review`, `db`, `poll`) depends on
[`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — a pure-Go SQLite driver, no
cgo, so cross-compiling and shipping a single static binary still works exactly as before
(`CGO_ENABLED=0 go build ./...` is part of this project's own verification). Being pure Go
means it's a real C-to-Go transpile of SQLite rather than a thin wrapper, so it pulls in
~10 supporting modules and noticeably lengthens build time — an accepted tradeoff for
staying cgo-free, not a bug.

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`, which builds a
`linux/amd64` binary (`make dist`) and attaches it to a GitHub release.

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
message wrote against the resolved calendar date. **A recurring event ("neljänä
perättäisenä viikkona") is expanded into one output record per occurrence** — the model
still returns one candidate with a `{freq,count}` hint, but Go turns that into N
independent dated records (stepping by the frequency from the first occurrence), each with
its own resolved date and its own review flags. There's no field linking them back into a
series; a weekly swim class becomes 4 plain events, not 1 event plus a recurrence blob.
Location and packing-list-style details are not their own fields either — the model is
asked to fold that prose into `detail` (or it's already sitting in the verbatim `quote`)
instead of parsing it into structured `location`/`items` fields.

With no `--db` flag, this is a pure stdin→stdout transform, no file written anywhere — the
mode this was originally built and tuned in, and still the right one for quick iteration on
the prompt (`internal/extract/prompt.go`) or schema from a shell. Add `--db <path>` to pull
from a persistent pending queue instead and save the results — see "Persistence" below.

### Credentials

```sh
export GEMINI_API_KEY=your-google-ai-studio-api-key
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
| `--api-key-env` | `GEMINI_API_KEY` | name of the env var holding the API key |
| `--model` | `gemini-3.5-flash-lite` | Gemini model id |
| `--base-url` | (real endpoint) | override, mainly for tests/a local proxy |
| `--delay` | `200ms` | pause between API calls |
| `--max-retries` | `3` | attempts on HTTP 429/5xx *within one call* before giving up on it; a 4xx other than 429 is never retried |
| `--db` | `$WILMA_DB` | SQLite path; enables persistent pending-queue mode instead of stdin (see "Persistence") |
| `--limit` | `20` | max pending messages to process this run (`--db` mode only) |
| `--max-attempts` | `5` | mark a message `failed` (stop retrying) after this many failed *runs*, not HTTP retries (`--db` mode only) |
| `--interval` | `0` | `0` = one pass then exit; `>0` (minimum `1m`) keeps draining the pending queue on that interval instead, the same idea as `poll --interval` (`--db` mode only) |
| `--note` | `""` | free text recorded on this pass's run row (default: `"pending queue"`); see "reextract" below for what a run is |
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
 "audience":"child","extract_ver":3}
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

A recurring candidate's occurrences share everything above except `resolved_date` (and
`weekday_ok`/`needs_review`/`review_reasons`, each computed independently per occurrence —
e.g. a monthly series can be weekday-consistent on one occurrence and not on the next, even
though the model only gave one `weekday_claim` for the whole series). `date_raw` stays the
same literal phrase on every occurrence — it's what the message actually said; only
`resolved_date` differs. There's no `series_id` or `occurrence` field: four occurrences of
one swim class are four unrelated-looking rows that happen to share a title and quote, by
design — see `internal/extract/validate.go`'s `BuildEvent` doc comment.

`model_confidence` is the model's self-reported score — **do not trust it**; it reported
`1.0` on the one live response where a schema bug caused it to silently drop data. All
`needs_review` decisions come from the deterministic Go validators, never from this field.

`audience` (added in `extract_ver` 2) is `"child"` (concerns the student — most events) or
`"guardians"` (concerns only the parents, e.g. a vanhempainilta/parents'-evening notice, not
the student). The model is deliberately never told which children received a message — see
"the child association comes from Wilma metadata, never from the model" below — it only
classifies who an event is *for*; in `--db` mode that classification is what decides the
`children` list `wilmabridge review` prints (see "Persistence" below). If the model omits or
sends an unrecognized value, `audience` defaults to `"child"` (the safe, pre-audience
behavior) and the event is flagged `needs_review` rather than silently guessed — same
philosophy as an unparseable date. **This field is newer and less validated than the rest of
the schema**: it was added on top of the (thoroughly live-tested) date/weekday/recurrence
logic without a live capture of a real guardians-only message yet — see
`internal/extract/testdata/README.md` for exactly what is and isn't verified.

## `reextract`: re-running extraction

```sh
# See what would be redone, at no cost -- no Gemini calls, nothing written.
wilmabridge reextract --db wilma.db --dry-run

# Catch messages up to the current prompt/schema version (extract_ver).
wilmabridge reextract --db wilma.db --limit 50

# Redo specific messages with a stronger model, even if already current.
wilmabridge reextract --db wilma.db --force --model gemini-3.5-pro --id 19790210
```

Every extraction pass — the everyday `extract --db` pending-queue drain and an explicit
`reextract` alike — is one **run**, recorded in the database (table `extraction_runs`).
**Latest run wins, and that's derived, not stored**: there is no "current"/"superseded" flag
anywhere. Re-extracting a message never deletes or edits its previous events — they just sit
there, older — and which events are "current" for a message is worked out at read time from
the highest-numbered run that covered it. `wilmabridge db runs` lists run history; `sqlite3
wilma.db` can always see every generation of every message's events if you need to compare.

By default `reextract` selects messages that are `done` or `failed` **and** whose stored
`extract_ver` is behind the binary's current one (a `failed` message with no `extract_ver` at
all always counts as behind) — so bumping the prompt/schema (as the `audience` field did,
ver 1 → 2) makes your whole history eligible with zero extra bookkeeping. `--force` ignores
version entirely, for redoing already-current messages with a different model. `extract --db`
(the pending queue) is untouched by any of this — the two commands never select the same
messages, so running both never double-extracts anything.

### Flags

| flag | default | meaning |
|---|---|---|
| `--db` | `$WILMA_DB` | required |
| `--api-key-env`, `--model`, `--base-url`, `--delay`, `--max-retries`, `--max-attempts`, `-v` | same as `extract` | |
| `--limit` | `20` | deliberately not unlimited — a re-run costs real money |
| `--force` | off | ignore `extract_ver`; redo even messages already at the current version |
| `--since` | `0` | only messages sent within this duration before now (`0` = no lower bound) |
| `--id` | — | restrict to one `wilma_id`; repeatable |
| `--note` | auto-generated | free text recorded on this run's row |
| `--dry-run` | off | print the selected messages (id, sent-at, subject) and exit — no Gemini calls, no run created |

`--dry-run` matters here specifically: a re-run against a paid or stronger model is exactly
the kind of thing worth previewing before it costs money.

## `poll`: automatic incremental sync

```sh
export WILMA_DB=wilma.db

# Cron-friendly: one pass, then exit. Put this on a schedule (e.g. */15 * * * *).
wilmabridge poll

# Or keep the process alive and let it poll itself:
wilmabridge poll --interval 15m
```

`sync` is stateless and `ingest` only records what it was handed, so keeping a database in
sync with Wilma otherwise means hand-rolling `sync --after $(wilmabridge db last-id --role
!X) | wilmabridge ingest` yourself — and that breaks down with more than one child, since
`sync --after` takes a single global ID floor while `db last-id` holds one per role. `poll`
does this bookkeeping internally, per role, every time it runs:

- **No recorded high-water mark for a role** (its first ever poll — including a newly added
  child) — fetch messages from the last `--bootstrap` window (default `720h` / 30 days).
  Nothing in Wilma supports filtering by date server-side, so this is done by scanning for
  the oldest message whose timestamp falls inside the window and taking everything from
  there up by ID (Wilma IDs increase monotonically with time, the same assumption `sync
  --after` relies on).
- **A recorded mark** — fetch only messages with a higher ID. Time never enters into it,
  which is what makes this resumable: an interrupted pass (crash, network blip, a `SIGTERM`
  from `--interval` mode) always picks back up exactly where it left off, never skipping a
  gap. The mark advances after each message is safely stored, not once at the end of a role.
- **A message's body fails to fetch** — that role stops there for this pass (the mark is
  *not* advanced past it) rather than skipping it and continuing; skipping would either
  permanently poison the message (an empty body is unextractable) or force it to be
  re-listed and re-attempted forever. The next `poll` retries automatically. If one message
  keeps failing and wedges a role, see `db set-last-id` below.

### Flags

| flag | default | meaning |
|---|---|---|
| `--host` | `$WILMA_HOST` | Wilma host |
| `--db` | `$WILMA_DB` | SQLite database path (required) |
| `--bootstrap` | `720h` | time window used only on a role's first ever poll (`0` disables the cutoff) |
| `--from-now` | off | on a role's first ever poll, adopt the newest message id and fetch nothing — skip the bootstrap window entirely |
| `--interval` | `0` | `0` = one pass then exit (the cron-friendly default); `>0` (minimum `1m`) keeps polling on that interval, re-logging in automatically if the session expires |
| `--role` | all | restrict to one role prefix (repeatable) |
| `--children` | `$WILMA_CHILDREN` | label map, same as `sync` |
| `--bodies` | `true` | fetch full message bodies — **see the warning below if you turn this off** |
| `--delay` | `300ms` | pause between HTTP requests |
| `-v` | off | log HTTP requests plus per-role/per-message detail to stderr |

> **`--bodies=false` warning.** Unlike `sync`, `poll` writes to a database that `extract`
> later reads from. A message ingested without a body is stored `extract_state=skipped`
> **and the high-water mark still advances past it** — so it becomes permanently invisible
> to `extract`, not just delayed. If you use `--bodies=false` for a non-invasive poll, plan
> to backfill bodies separately when you're ready to extract:
> ```sh
> wilmabridge sync --since 720h | wilmabridge ingest
> ```

`poll` only fetches and stores messages — it never runs extraction. Run `wilmabridge
extract --db` separately (its own schedule, its own Gemini quota):

```sh
wilmabridge poll --db wilma.db
wilmabridge extract --db wilma.db
```

A clean pass with nothing new prints nothing (cron-friendly — output should mean something
happened). `-v` always prints a per-role and totals summary, useful as a heartbeat under
`--interval`.

### Limitations

- Inbox only — there's no `--folder` flag, because the high-water mark is keyed by role
  prefix alone and a second folder would need its own mark namespace. A message archived
  out of the inbox between two passes is missed.
- `--interval` shutdown (`Ctrl-C` / `SIGTERM`) waits for the current HTTP request to finish
  before exiting (bounded by the client's 30s timeout) — it isn't instant.
- `wilmabridge db set-last-id --db <path> --role <prefix> <id>` manually advances a role's
  mark — the escape hatch for a role stuck behind one message that keeps failing to fetch.
  It refuses to move a mark backward (same guarantee `poll` itself relies on), so it can't
  be used to accidentally force a re-fetch of history.

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
- **`wilmabridge review --db <path>`** — NDJSON of every **current** event (see "reextract"
  above for what makes an event current) flagged `needs_review`, annotated with which
  children it concerns. The child association comes from Wilma metadata, never from the
  model: extraction runs once per message regardless of how many kids received it, and each
  event's `audience` classification (see `extract`'s output record above) decides whether
  that fans out to every child linked to the message (`"child"`) or to none at all
  (`"guardians"`) — `children` is always present, `[]` rather than omitted when it's empty,
  so "deliberately zero" is never confused with "not computed".
- **`wilmabridge db last-id --db <path> [--role <prefix>]`** — prints the stored per-role
  high-water mark and when that role was last polled. Historically the way to feed `sync
  --after` for a cron job (`sync --after $(wilmabridge db last-id --role !X) | ingest`);
  that bookkeeping is now handled internally by **`poll`** (see above), which is the
  recommended way to keep a database in sync going forward. The manual `sync --after`
  recipe still works for a one-off backfill.
- **`wilmabridge db set-last-id --db <path> --role <prefix> <id>`** — manually advances a
  role's high-water mark; see `poll`'s "Limitations" above for when you'd need this.
- **`wilmabridge db runs --db <path> [--limit N]`** — NDJSON of extraction run history,
  newest first (model, `extract_ver`, when it started/finished, how many messages/events it
  produced) — see "reextract" above.

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

For `poll --interval`: a `Ctrl-C`/`SIGTERM` shutdown exits `0` even mid-pass — it was
requested, not a crash. A single pass failing (a role error, a transient network issue)
is logged and the loop continues to the next tick rather than exiting; only a setup
failure (bad `--host`/`--db`/credentials) or five consecutive failed re-logins after a
session expiry causes `--interval` mode itself to exit non-zero.

For `extract --interval`: same shutdown behavior (`0` on signal, even mid-pass — an
in-flight Gemini call is not counted as a failed attempt when cancelled this way, so it
doesn't burn a retry). A pass that fails outright is logged and the loop waits for the next
tick; three *consecutive* per-message failures within one pass (a dead API key, a Gemini
outage) abort just that pass early rather than spending every remaining message's retry
budget on a cause that has nothing to do with the messages themselves.
