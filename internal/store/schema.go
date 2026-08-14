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

CREATE TABLE IF NOT EXISTS events (
  id               INTEGER PRIMARY KEY,
  wilma_id         INTEGER NOT NULL,
  content_hash     TEXT    NOT NULL,
  kind             TEXT    NOT NULL,
  title            TEXT    NOT NULL,
  detail           TEXT,
  date_raw         TEXT    NOT NULL,
  weekday_claim    TEXT,
  time             TEXT,
  location         TEXT,
  items            TEXT,
  link             TEXT,
  recurrence       TEXT,
  model_confidence REAL,
  quote            TEXT    NOT NULL,
  resolved_date    TEXT,
  weekday_ok       INTEGER,
  needs_review     INTEGER NOT NULL DEFAULT 0,
  review_reasons   TEXT,
  extract_ver      INTEGER NOT NULL,
  created_at       TEXT    NOT NULL,
  FOREIGN KEY (wilma_id, content_hash) REFERENCES messages (wilma_id, content_hash)
);

CREATE INDEX IF NOT EXISTS idx_events_needs_review ON events (needs_review);
CREATE INDEX IF NOT EXISTS idx_events_message ON events (wilma_id, content_hash);

CREATE TABLE IF NOT EXISTS extractions (
  id            INTEGER PRIMARY KEY,
  wilma_id      INTEGER NOT NULL,
  content_hash  TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  request       TEXT    NOT NULL,
  response      TEXT,
  status_code   INTEGER,
  http_attempts INTEGER,
  error         TEXT,
  created_at    TEXT    NOT NULL
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
