package store

// schemaSQL creates every table wilmabridge persists to, idempotently
// (IF NOT EXISTS) so Open can just run this on every startup with no
// separate migration step or version tracking yet.
//
// Column names deliberately mirror the real Go struct fields in
// internal/wilma.Message and internal/extract.Event rather than the earlier
// sketch in openclaw-integration.md, which predates this implementation and
// drifted in a few places (e.g. that doc has no weekday_ok column because
// the field didn't exist yet when it was written).
const schemaSQL = `
CREATE TABLE IF NOT EXISTS messages (
  wilma_id      INTEGER NOT NULL,
  content_hash  TEXT    NOT NULL,
  subject       TEXT    NOT NULL,
  sender        TEXT,
  sender_id     INTEGER,
  folder        TEXT,
  sent_at       TEXT,
  sent_at_raw   TEXT    NOT NULL,
  body_text     TEXT,
  body_html     TEXT,
  url           TEXT    NOT NULL,
  fetched_at    TEXT    NOT NULL,
  extract_state TEXT    NOT NULL DEFAULT 'pending',
  extract_ver   INTEGER,
  attempts      INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT,
  PRIMARY KEY (wilma_id, content_hash)
);

CREATE INDEX IF NOT EXISTS idx_messages_extract_state ON messages (extract_state);

CREATE TABLE IF NOT EXISTS message_children (
  wilma_id     INTEGER NOT NULL,
  content_hash TEXT    NOT NULL,
  child        TEXT    NOT NULL,
  role_prefix  TEXT    NOT NULL,
  was_unread   INTEGER NOT NULL,
  PRIMARY KEY (wilma_id, content_hash, role_prefix),
  FOREIGN KEY (wilma_id, content_hash) REFERENCES messages (wilma_id, content_hash)
);

-- events.run_id and events.audience are deliberately declared bare here
-- (no NOT NULL, no DEFAULT, no REFERENCES) even though every row written by
-- current code always sets both: a database that predates run tracking gets
-- these columns added later via ALTER TABLE ADD COLUMN (see ensureColumn in
-- store.go), which cannot express NOT NULL without a DEFAULT or REFERENCES
-- with a non-NULL default. Declaring them identically bare in both places is
-- what keeps a fresh database's schema and a migrated one's byte-identical.
--
-- No location/items/recurrence columns: as of extract.ExtractVer 3, that
-- information lives in "detail" (or "quote") instead of its own field, and
-- a recurring candidate is expanded into one row per occurrence rather
-- than one row carrying a {freq,count} blob (see internal/extract's
-- validate.go). A database created before ver 3 may still have these three
-- columns from an earlier schemaSQL; they're simply never read or written
-- by current code. Deliberately not dropped -- this project makes only
-- additive schema changes (see ensureColumn/migrate below) and a DROP
-- COLUMN on someone's real local data is not a call to make silently.
CREATE TABLE IF NOT EXISTS events (
  id               INTEGER PRIMARY KEY,
  wilma_id         INTEGER NOT NULL,
  content_hash     TEXT    NOT NULL,
  run_id           INTEGER,
  kind             TEXT    NOT NULL,
  title            TEXT    NOT NULL,
  detail           TEXT,
  date_raw         TEXT    NOT NULL,
  weekday_claim    TEXT,
  time             TEXT,
  link             TEXT,
  model_confidence REAL,
  quote            TEXT    NOT NULL,
  resolved_date    TEXT,
  weekday_ok       INTEGER,
  needs_review     INTEGER NOT NULL DEFAULT 0,
  review_reasons   TEXT,
  audience         TEXT,
  extract_ver      INTEGER NOT NULL,
  created_at       TEXT    NOT NULL,
  FOREIGN KEY (wilma_id, content_hash) REFERENCES messages (wilma_id, content_hash)
);

CREATE INDEX IF NOT EXISTS idx_events_needs_review ON events (needs_review);
CREATE INDEX IF NOT EXISTS idx_events_message ON events (wilma_id, content_hash);
-- idx_events_message_run is NOT created here: on a database that predates
-- run_id (see the comment above), this CREATE INDEX would run before
-- migrate() has had a chance to ALTER TABLE the column in, and fail with
-- "no such column: run_id". It's created in migrate() instead, after
-- ensureColumn guarantees the column exists either way.

-- extractions.run_id: same bare-declaration reasoning as events.run_id above.
CREATE TABLE IF NOT EXISTS extractions (
  id            INTEGER PRIMARY KEY,
  wilma_id      INTEGER NOT NULL,
  content_hash  TEXT    NOT NULL,
  run_id        INTEGER,
  model         TEXT    NOT NULL,
  request       TEXT    NOT NULL,
  response      TEXT,
  status_code   INTEGER,
  http_attempts INTEGER,
  error         TEXT,
  created_at    TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_extractions_message ON extractions (wilma_id, content_hash);

-- extraction_runs records every extraction pass -- the everyday pending-queue
-- drain and an explicit "reextract" alike. There is deliberately no
-- "current"/"superseded" flag anywhere in this schema: the latest generation
-- of events for a message is always derived at query time from the highest
-- run_id that covered it (see run_messages and Store.LatestEvents), never a
-- column that has to be kept in sync. Old runs are never deleted or mutated
-- -- they just sit there, older.
--
-- id is AUTOINCREMENT rather than a bare rowid alias because the ordering
-- guarantee matters: a plain rowid can be reused after the highest-numbered
-- row is deleted, which would make a new run sort before an older one.
CREATE TABLE IF NOT EXISTS extraction_runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  model       TEXT    NOT NULL,
  extract_ver INTEGER NOT NULL,
  note        TEXT,
  started_at  TEXT    NOT NULL,
  finished_at TEXT
);

-- run_messages records which messages a run covered, independent of whether
-- that run produced any events for them. This is what makes "latest run
-- wins" correct in the zero-event case: if a re-run reads a message and
-- (correctly) finds no dated events, the message must end up with no current
-- events -- deriving coverage from the events table alone would instead
-- resurrect the previous run's events, because MAX(run_id) over zero rows
-- falls back to the older generation. Written in the same transaction as the
-- events themselves, so the two can never disagree after a crash.
CREATE TABLE IF NOT EXISTS run_messages (
  run_id       INTEGER NOT NULL,
  wilma_id     INTEGER NOT NULL,
  content_hash TEXT    NOT NULL,
  PRIMARY KEY (run_id, wilma_id, content_hash),
  FOREIGN KEY (run_id) REFERENCES extraction_runs (id),
  FOREIGN KEY (wilma_id, content_hash) REFERENCES messages (wilma_id, content_hash)
);

CREATE INDEX IF NOT EXISTS idx_run_messages_message ON run_messages (wilma_id, content_hash, run_id);

-- event_children is the resolved per-event audience: which of this account's
-- children an event actually concerns. audience='child' fans out to every
-- child in message_children for the source message (a school-wide message
-- already has one row per child there -- that's the existing dedup
-- mechanism, so "whole school" falls out for free); audience='guardians'
-- produces ZERO rows here, which is exactly the "event targeted at the
-- parents, not the student" case. The model never sees child names: it only
-- classifies (see internal/extract), and this table is where Go resolves
-- that classification into an actual list.
CREATE TABLE IF NOT EXISTS event_children (
  event_id INTEGER NOT NULL,
  child    TEXT    NOT NULL,
  PRIMARY KEY (event_id, child),
  FOREIGN KEY (event_id) REFERENCES events (id)
);

CREATE TABLE IF NOT EXISTS sync_state (
  role       TEXT PRIMARY KEY,
  last_id    INTEGER NOT NULL,
  updated_at TEXT    NOT NULL
);

-- poll_state records when poll last successfully reached each role,
-- independent of sync_state's high-water mark. Kept as its own table
-- (rather than a column added to sync_state) so a poll that ran but found
-- nothing new can never be mistaken for a high-water mark of 0 -- which
-- would make HighWaterMark report the role as already having a mark, and
-- so skip the bootstrap window and fetch (and mark read) the entire
-- folder. It also means a plain "IF NOT EXISTS" is enough for a database
-- created by an older wilmabridge: no ALTER TABLE migration needed.
CREATE TABLE IF NOT EXISTS poll_state (
  role           TEXT PRIMARY KEY,
  last_polled_at TEXT NOT NULL
);
`
