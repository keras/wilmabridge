// Package store persists what sync/extract produce into a local SQLite
// file: messages (deduplicated across the children they were sent to),
// the events extracted from them, and an audit trail of every Gemini call.
// See schema.go for the table definitions and openclaw-integration.md for
// the design this implements (minus the reminders/scheduling layer, which
// this package deliberately does not build yet).
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"wilmabridge/internal/extract"
	"wilmabridge/internal/gemini"
	"wilmabridge/internal/wilma"
)

// Store is a handle to one wilmabridge SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path and
// ensures its schema exists. Callers must Close it when done.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty path")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	// SQLite tolerates only one writer at a time; keeping the pool to a
	// single connection avoids SQLITE_BUSY from this process's own
	// goroutines racing each other (there are none today, but this is the
	// cheap, correct default for a single-writer embedded DB).
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}

	s := &Store{db: db}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrating schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrating schema: %w", err)
	}
	return s, nil
}

// ensureColumn adds column to table if it isn't there yet, reporting whether
// it actually added it. schemaSQL's CREATE TABLE IF NOT EXISTS is a no-op
// against a table that already exists, so a new column on an old table needs
// this; ALTER TABLE ADD COLUMN is not itself idempotent, hence the
// pragma_table_info check first.
//
// Uses QueryRow, never Query: Open pins the pool to a single connection (see
// SetMaxOpenConns(1) above), so holding an open *sql.Rows across the Exec
// below would deadlock this process against its own only connection.
func ensureColumn(db *sql.DB, table, column, decl string) (added bool, err error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("inspecting %s.%s: %w", table, column, err)
	}
	if n > 0 {
		return false, nil
	}
	if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl); err != nil {
		return false, fmt.Errorf("adding %s.%s: %w", table, column, err)
	}
	return true, nil
}

// migrate applies the additive changes CREATE TABLE IF NOT EXISTS cannot
// express (new columns on tables that already existed before run tracking),
// and backfills them so no query anywhere has to special-case a NULL run_id
// or audience. Idempotent: called on every Open, a no-op once a database has
// already been migrated once. See schema.go's comment on events.run_id for
// why the declarations here must match schemaSQL's exactly.
func migrate(db *sql.DB) error {
	addedEventsRunID, err := ensureColumn(db, "events", "run_id", "INTEGER")
	if err != nil {
		return err
	}
	addedAudience, err := ensureColumn(db, "events", "audience", "TEXT")
	if err != nil {
		return err
	}
	addedExtractionsRunID, err := ensureColumn(db, "extractions", "run_id", "INTEGER")
	if err != nil {
		return err
	}
	// Safe to create unconditionally and every Open: IF NOT EXISTS makes it
	// a no-op once it exists, and by this point events.run_id is guaranteed
	// present whether this database is fresh or was just migrated.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_message_run ON events (wilma_id, content_hash, run_id)`); err != nil {
		return fmt.Errorf("creating idx_events_message_run: %w", err)
	}
	if !addedEventsRunID && !addedAudience && !addedExtractionsRunID {
		return nil
	}

	// This database predates run tracking: everything currently in events/
	// extractions is "legacy" (its run_id/audience just went from
	// nonexistent to NULL). Attach it all to one synthetic run so
	// LatestEvents' run_messages-based derivation and the old implicit
	// "fan out to every recipient" behavior both keep working unchanged.
	// One transaction: a crash partway through must not leave events
	// attached to a run that run_messages doesn't know covered them, or
	// vice versa.
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("starting migration backfill: %w", err)
	}
	defer tx.Rollback()

	now := nowRFC3339()
	res, err := tx.Exec(`
		INSERT INTO extraction_runs (model, extract_ver, note, started_at, finished_at)
		VALUES ('(unknown)', 1, 'backfill: extracted before run tracking existed', ?, ?)`,
		now, now,
	)
	if err != nil {
		return fmt.Errorf("inserting backfill run: %w", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("reading backfill run id: %w", err)
	}

	if _, err := tx.Exec(`UPDATE events SET run_id = ? WHERE run_id IS NULL`, runID); err != nil {
		return fmt.Errorf("backfilling events.run_id: %w", err)
	}
	if _, err := tx.Exec(`UPDATE extractions SET run_id = ? WHERE run_id IS NULL`, runID); err != nil {
		return fmt.Errorf("backfilling extractions.run_id: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO run_messages (run_id, wilma_id, content_hash)
		SELECT ?, wilma_id, content_hash FROM events WHERE run_id = ?`,
		runID, runID,
	); err != nil {
		return fmt.Errorf("backfilling run_messages: %w", err)
	}
	if _, err := tx.Exec(`UPDATE events SET audience = 'child' WHERE audience IS NULL`); err != nil {
		return fmt.Errorf("backfilling events.audience: %w", err)
	}
	// Legacy events implicitly concerned every child on the source message
	// (today's pre-audience behavior) -- replicate that as explicit
	// event_children rows so NeedsReview's output doesn't change shape
	// across the migration.
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO event_children (event_id, child)
		SELECT e.id, mc.child
		FROM events e
		JOIN message_children mc ON mc.wilma_id = e.wilma_id AND mc.content_hash = e.content_hash
		WHERE e.audience = 'child'`,
	); err != nil {
		return fmt.Errorf("backfilling event_children: %w", err)
	}

	return tx.Commit()
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// contentHash identifies a message's content independent of which child's
// inbox it was fetched from, so the same Wilma message arriving via two
// children's role feeds collapses to one row. Built from sent_at_raw (not
// the parsed time.Time) because wilma.Message guarantees the raw string is
// always present even when Wilma's timestamp failed to parse.
func contentHash(subject, sentAtRaw, bodyHTML string) string {
	h := sha256.New()
	h.Write([]byte(subject))
	h.Write([]byte{0x1f})
	h.Write([]byte(sentAtRaw))
	h.Write([]byte{0x1f})
	h.Write([]byte(bodyHTML))
	return hex.EncodeToString(h.Sum(nil))
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// jsonOrNil marshals v to a JSON string for storage, or returns nil when
// empty tells it there's nothing to store — used instead of checking v
// itself, since a nil *T stored in an `any` is not itself == nil.
func jsonOrNil(v any, empty bool) (any, error) {
	if empty {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// IngestMessage upserts one sync-produced message. isNew reports whether
// this created a new messages row (false when this is the same content
// arriving via a second child, or a genuine re-ingest of something already
// known) — the message_children link is always recorded either way, which
// is what makes a message sent to both kids fan out to two children while
// still being extracted only once.
func (s *Store) IngestMessage(msg wilma.Message) (isNew bool, err error) {
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	var sentAt any
	if !msg.SentAt.IsZero() {
		sentAt = msg.SentAt.UTC().Format(time.RFC3339)
	}

	extractState := "pending"
	if strings.TrimSpace(msg.BodyText) == "" {
		// Mirrors the existing stdin-mode skip in cmdExtract: a body-less
		// message (fetched with sync --bodies=false) can never be
		// extracted, so don't leave it sitting in the pending queue.
		extractState = "skipped"
	}

	res, err := s.db.Exec(`
		INSERT INTO messages
			(wilma_id, content_hash, subject, sender, sender_id, folder,
			 sent_at, sent_at_raw, body_text, body_html, url, fetched_at, extract_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (wilma_id, content_hash) DO NOTHING`,
		msg.ID, hash, msg.Subject, nullString(msg.Sender), nullInt64(msg.SenderID), nullString(msg.Folder),
		sentAt, msg.SentAtRaw, nullString(msg.BodyText), nullString(msg.BodyHTML), msg.URL,
		nowRFC3339(), extractState,
	)
	if err != nil {
		return false, fmt.Errorf("store: ingesting message %d: %w", msg.ID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		isNew = true
	}

	if _, err := s.db.Exec(`
		INSERT INTO message_children (wilma_id, content_hash, child, role_prefix, was_unread)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (wilma_id, content_hash, role_prefix) DO UPDATE SET was_unread = excluded.was_unread`,
		msg.ID, hash, msg.Child, msg.Role, boolToInt(msg.Unread),
	); err != nil {
		return isNew, fmt.Errorf("store: linking message %d to child %q: %w", msg.ID, msg.Child, err)
	}

	return isNew, nil
}

// PendingMessage is enough of a message to build a Gemini extraction
// request from. It deliberately carries no child/role: a message may have
// been sent to multiple children (see message_children), and extraction
// runs once per message regardless — see the plan's "Why extraction runs
// once per message, not once per child".
type PendingMessage struct {
	WilmaID     int64
	ContentHash string
	Subject     string
	BodyText    string
	SentAt      time.Time
	SentAtRaw   string
	URL         string
}

// PendingMessages returns up to limit messages awaiting extraction,
// oldest first.
func (s *Store) PendingMessages(limit int) ([]PendingMessage, error) {
	rows, err := s.db.Query(`
		SELECT wilma_id, content_hash, subject, COALESCE(body_text, ''), sent_at, sent_at_raw, url
		FROM messages
		WHERE extract_state = 'pending'
		ORDER BY wilma_id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: querying pending messages: %w", err)
	}
	defer rows.Close()

	var out []PendingMessage
	for rows.Next() {
		var pm PendingMessage
		var sentAt sql.NullString
		if err := rows.Scan(&pm.WilmaID, &pm.ContentHash, &pm.Subject, &pm.BodyText, &sentAt, &pm.SentAtRaw, &pm.URL); err != nil {
			return nil, fmt.Errorf("store: scanning pending message: %w", err)
		}
		if sentAt.Valid {
			if t, err := time.Parse(time.RFC3339, sentAt.String); err == nil {
				pm.SentAt = t
			}
		}
		out = append(out, pm)
	}
	return out, rows.Err()
}

// Run is one extraction pass — the everyday pending-queue drain or an
// explicit reextract alike. See schema.go's extraction_runs comment: there
// is no "current" flag anywhere in this schema, so a Run is never deleted
// or mutated except to stamp FinishedAt.
type Run struct {
	ID         int64     `json:"id"`
	Model      string    `json:"model"`
	ExtractVer int       `json:"extract_ver"`
	Note       string    `json:"note,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Messages   int       `json:"messages"`
	Events     int       `json:"events"`
}

// MarshalJSON customizes FinishedAt the same way wilma.Message customizes
// SentAt: struct-typed omitempty is a no-op in encoding/json (a zero
// time.Time is never "empty" to it), so without this a run left unfinished
// (crash, kill, a pipe closed early) would print "finished_at":
// "0001-01-01T00:00:00Z" instead of omitting the field.
func (r Run) MarshalJSON() ([]byte, error) {
	type alias Run
	aux := struct {
		FinishedAt string `json:"finished_at,omitempty"`
		alias
	}{alias: alias(r)}
	if !r.FinishedAt.IsZero() {
		aux.FinishedAt = r.FinishedAt.Format(time.RFC3339)
	}
	return json.Marshal(aux)
}

// StartRun opens an extraction run and returns its id. Every extraction
// pass — the everyday pending-queue drain as much as an explicit
// reextract — is one run; note is free text for a human reading the audit
// trail later (e.g. "pending queue" or "reextract --force --model
// gemini-3.5-pro").
func (s *Store) StartRun(model string, extractVer int, note string) (runID int64, err error) {
	res, err := s.db.Exec(`
		INSERT INTO extraction_runs (model, extract_ver, note, started_at)
		VALUES (?, ?, ?, ?)`,
		model, extractVer, nullString(note), nowRFC3339(),
	)
	if err != nil {
		return 0, fmt.Errorf("store: starting run: %w", err)
	}
	runID, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: reading new run id: %w", err)
	}
	return runID, nil
}

// FinishRun stamps finished_at. A run left unfinished (crash, kill) is
// still a perfectly valid generation of events — nothing in LatestEvents'
// derivation looks at finished_at; it exists purely for the human reading
// `db runs`.
func (s *Store) FinishRun(runID int64) error {
	_, err := s.db.Exec(`UPDATE extraction_runs SET finished_at = ? WHERE id = ?`, nowRFC3339(), runID)
	if err != nil {
		return fmt.Errorf("store: finishing run %d: %w", runID, err)
	}
	return nil
}

// Runs returns the most recent extraction runs, newest first, with a
// per-run count of messages covered and events produced.
func (s *Store) Runs(limit int) ([]Run, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.model, r.extract_ver, r.note, r.started_at, r.finished_at,
		       (SELECT COUNT(*) FROM run_messages rm WHERE rm.run_id = r.id),
		       (SELECT COUNT(*) FROM events e WHERE e.run_id = r.id)
		FROM extraction_runs r
		ORDER BY r.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: querying runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		var note, finishedAt sql.NullString
		var startedAt string
		if err := rows.Scan(&r.ID, &r.Model, &r.ExtractVer, &note, &startedAt, &finishedAt, &r.Messages, &r.Events); err != nil {
			return nil, fmt.Errorf("store: scanning run: %w", err)
		}
		r.Note = note.String
		if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
			r.StartedAt = t
		}
		if finishedAt.Valid {
			if t, err := time.Parse(time.RFC3339, finishedAt.String); err == nil {
				r.FinishedAt = t
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveExtraction records one Gemini call attempt — successful or not — for
// audit and later replay. Uses gemini.RawExchange, which ExtractMessage
// already computes; before this package existed, cmdExtract discarded it.
func (s *Store) SaveExtraction(runID, wilmaID int64, contentHash, model string, ex gemini.RawExchange, callErr error) error {
	var errMsg any
	if callErr != nil {
		errMsg = callErr.Error()
	}
	_, err := s.db.Exec(`
		INSERT INTO extractions
			(wilma_id, content_hash, run_id, model, request, response, status_code, http_attempts, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		wilmaID, contentHash, runID, model, string(ex.Request), nullString(string(ex.Response)),
		nullInt64(int64(ex.StatusCode)), nullInt64(int64(ex.Attempts)), errMsg, nowRFC3339(),
	)
	if err != nil {
		return fmt.Errorf("store: saving extraction for message %d: %w", wilmaID, err)
	}
	return nil
}

// messageChildren returns every child a message was sent to, per
// message_children.
func (s *Store) messageChildren(wilmaID int64, contentHash string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT child FROM message_children
		WHERE wilma_id = ? AND content_hash = ?
		ORDER BY child`, wilmaID, contentHash)
	if err != nil {
		return nil, fmt.Errorf("store: querying message_children for message %d: %w", wilmaID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return nil, fmt.Errorf("store: scanning message_children for message %d: %w", wilmaID, err)
		}
		out = append(out, child)
	}
	return out, rows.Err()
}

// SaveEvents persists one message's extraction result as part of run runID:
// the events, their resolved event_children audience, and — crucially — a
// run_messages row recording that this run covered this message AT ALL.
// Call once per successful extraction; MarkExtractDone should follow.
//
// Must be called even when events is empty: an empty result is a real
// result that has to override the previous run's events for this message
// (see schema.go's run_messages comment for why deriving "current" from
// MAX(events.run_id) alone would get this wrong). Do not add a
// len(events)==0 fast path that skips writing run_messages.
//
// Everything happens in one transaction, so a crash can never leave a
// coverage row without its events, or an event without its children.
func (s *Store) SaveEvents(runID, wilmaID int64, contentHash string, events []extract.Event) error {
	// Resolve child membership before opening the transaction: Open pins
	// the connection pool to 1 (see SetMaxOpenConns(1)), so a query issued
	// on s.db while a *sql.Tx is open would deadlock waiting for the
	// connection the Tx itself holds.
	children, err := s.messageChildren(wilmaID, contentHash)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: starting transaction for message %d: %w", wilmaID, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO run_messages (run_id, wilma_id, content_hash) VALUES (?, ?, ?)`,
		runID, wilmaID, contentHash,
	); err != nil {
		return fmt.Errorf("store: recording run coverage for message %d: %w", wilmaID, err)
	}

	now := nowRFC3339()
	for _, ev := range events {
		reasonsJSON, err := jsonOrNil(ev.ReviewReasons, len(ev.ReviewReasons) == 0)
		if err != nil {
			return fmt.Errorf("store: encoding review_reasons for message %d: %w", wilmaID, err)
		}
		var weekdayOK any
		if ev.WeekdayOK != nil {
			weekdayOK = boolToInt(*ev.WeekdayOK)
		}
		audience := audienceOrChild(ev.Audience)

		res, err := tx.Exec(`
			INSERT INTO events
				(wilma_id, content_hash, run_id, kind, title, detail, date_raw, weekday_claim, time,
				 link, model_confidence, quote, resolved_date,
				 weekday_ok, needs_review, review_reasons, audience, extract_ver, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			wilmaID, contentHash, runID, ev.Kind, ev.Title, nullString(ev.Detail), ev.DateRaw,
			nullString(ev.WeekdayClaim), nullString(ev.Time),
			nullString(ev.Link), ev.ModelConfidence, ev.Quote,
			nullString(ev.ResolvedDate), weekdayOK, boolToInt(ev.NeedsReview), reasonsJSON,
			audience, ev.ExtractVer, now,
		)
		if err != nil {
			return fmt.Errorf("store: saving event %q for message %d: %w", ev.Title, wilmaID, err)
		}
		eventID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: reading id for event %q on message %d: %w", ev.Title, wilmaID, err)
		}

		if audience == extract.AudienceChild {
			for _, child := range children {
				if _, err := tx.Exec(`
					INSERT OR IGNORE INTO event_children (event_id, child) VALUES (?, ?)`,
					eventID, child,
				); err != nil {
					return fmt.Errorf("store: linking event %d to child %q: %w", eventID, child, err)
				}
			}
		}
		// audience == guardians: deliberately zero event_children rows.
	}

	return tx.Commit()
}

// audienceOrChild is SaveEvents' defensive twin of BuildEvent's default: an
// Event arriving with no audience set (e.g. built by something other than
// extract.BuildEvent) is stored as "child" rather than as a NULL nothing
// downstream knows how to interpret.
func audienceOrChild(a string) string {
	if a == extract.AudienceGuardians {
		return extract.AudienceGuardians
	}
	return extract.AudienceChild
}

// MarkExtractDone transitions a message to 'done' after its events (zero or
// more) have been saved successfully. attempts resets to 0: a message that
// failed earlier attempts under a previous run (or an earlier reextract)
// starts its next retry budget fresh rather than carrying failures forward
// across unrelated runs.
func (s *Store) MarkExtractDone(wilmaID int64, contentHash string, extractVer int) error {
	_, err := s.db.Exec(`
		UPDATE messages SET extract_state = 'done', extract_ver = ?, last_error = NULL, attempts = 0
		WHERE wilma_id = ? AND content_hash = ?`,
		extractVer, wilmaID, contentHash,
	)
	if err != nil {
		return fmt.Errorf("store: marking message %d done: %w", wilmaID, err)
	}
	return nil
}

// MarkExtractFailed records a failed extraction attempt. The message stays
// 'pending' (so a later run retries it) until attempts reaches maxAttempts,
// at which point it becomes 'failed' and PendingMessages stops returning
// it — the retry budget is spent, not infinite.
func (s *Store) MarkExtractFailed(wilmaID int64, contentHash, errMsg string, maxAttempts int) error {
	_, err := s.db.Exec(`
		UPDATE messages
		SET attempts = attempts + 1,
		    last_error = ?,
		    extract_state = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE 'pending' END
		WHERE wilma_id = ? AND content_hash = ?`,
		errMsg, maxAttempts, wilmaID, contentHash,
	)
	if err != nil {
		return fmt.Errorf("store: marking message %d failed: %w", wilmaID, err)
	}
	return nil
}

// EventRow is a stored event annotated with what the events table alone
// doesn't carry: which run produced it and which children it targets (via
// event_children).
type EventRow struct {
	extract.Event
	EventID     int64    `json:"event_id"`
	RunID       int64    `json:"run_id"`
	ContentHash string   `json:"-"` // internal join key; not part of the public NDJSON shape
	Children    []string `json:"children"`
}

// ReviewRow is an EventRow returned by NeedsReview. Same shape — kept as a
// distinct name at call sites for readability, since "review" is a narrower,
// more specific idea than "the latest events".
type ReviewRow = EventRow

// EventFilter narrows LatestEvents. The zero value matches every current
// event.
type EventFilter struct {
	NeedsReviewOnly bool
	Limit           int // 0 = no limit
}

// LatestEvents returns each message's CURRENT events: the ones produced by
// the highest-numbered run that covered that message. "Current" is derived
// here, at query time, from run_messages — there is no current/superseded
// column stored anywhere to keep in sync, and re-running extraction never
// deletes or mutates an older run's rows; they are simply no longer latest.
// See schema.go's run_messages comment for why the join is against
// run_messages and not MAX(events.run_id) directly (a re-run that finds
// zero events must still supersede the previous run's events, which a bare
// MAX over the events table would get wrong).
func (s *Store) LatestEvents(f EventFilter) ([]EventRow, error) {
	query := `
		WITH latest AS (
			SELECT wilma_id, content_hash, MAX(run_id) AS run_id
			FROM run_messages
			GROUP BY wilma_id, content_hash
		)
		SELECT e.id, e.run_id, e.wilma_id, e.content_hash, e.kind, e.title, e.detail, e.date_raw,
		       e.weekday_claim, e.time, e.link,
		       e.model_confidence, e.quote, e.resolved_date, e.weekday_ok, e.needs_review,
		       e.review_reasons, e.audience, e.extract_ver, m.subject, m.url
		FROM latest l
		JOIN events e ON e.wilma_id = l.wilma_id AND e.content_hash = l.content_hash AND e.run_id = l.run_id
		JOIN messages m ON m.wilma_id = e.wilma_id AND m.content_hash = e.content_hash`
	var args []any
	if f.NeedsReviewOnly {
		query += ` WHERE e.needs_review = 1`
	}
	query += ` ORDER BY e.id DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: querying latest events: %w", err)
	}
	defer rows.Close()

	var out []EventRow
	for rows.Next() {
		var r EventRow
		var detail, weekdayClaim, evTime, link, resolvedDate, reasonsJSON, audience sql.NullString
		var weekdayOK sql.NullInt64
		var needsReview int64
		var confidence sql.NullFloat64
		if err := rows.Scan(
			&r.EventID, &r.RunID, &r.WilmaID, &r.ContentHash, &r.Kind, &r.Title, &detail, &r.DateRaw,
			&weekdayClaim, &evTime, &link,
			&confidence, &r.Quote, &resolvedDate, &weekdayOK, &needsReview, &reasonsJSON,
			&audience, &r.ExtractVer, &r.MessageSubject, &r.MessageURL,
		); err != nil {
			return nil, fmt.Errorf("store: scanning event: %w", err)
		}
		r.Detail = detail.String
		r.WeekdayClaim = weekdayClaim.String
		r.Time = evTime.String
		r.Link = link.String
		r.ResolvedDate = resolvedDate.String
		r.ModelConfidence = confidence.Float64
		r.NeedsReview = needsReview != 0
		r.Audience = audienceOrChild(audience.String)
		r.Children = []string{} // never nil: audience=guardians must marshal as [], not null

		if reasonsJSON.Valid {
			if err := json.Unmarshal([]byte(reasonsJSON.String), &r.ReviewReasons); err != nil {
				return nil, fmt.Errorf("store: decoding review_reasons for event %d: %w", r.EventID, err)
			}
		}
		if weekdayOK.Valid {
			ok := weekdayOK.Int64 != 0
			r.WeekdayOK = &ok
		}

		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := make([]int64, len(out))
	for i, r := range out {
		ids[i] = r.EventID
	}
	children, err := s.childrenForEvents(ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if c, ok := children[out[i].EventID]; ok {
			out[i].Children = c
		}
	}
	return out, nil
}

// NeedsReview returns every CURRENT event flagged needs_review, most recent
// first. Only the latest run per message is considered — a re-extracted
// message shows one generation of review items, not two.
func (s *Store) NeedsReview() ([]ReviewRow, error) {
	return s.LatestEvents(EventFilter{NeedsReviewOnly: true})
}

// childrenForEvents returns each event's resolved audience (event_children),
// keyed by event id. Events with no rows (audience=guardians) are simply
// absent from the map — callers should treat a missing key as an empty,
// non-nil list, matching LatestEvents' default.
func (s *Store) childrenForEvents(eventIDs []int64) (map[int64][]string, error) {
	if len(eventIDs) == 0 {
		return map[int64][]string{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventIDs)), ",")
	args := make([]any, len(eventIDs))
	for i, id := range eventIDs {
		args[i] = id
	}
	rows, err := s.db.Query(
		`SELECT event_id, child FROM event_children WHERE event_id IN (`+placeholders+`) ORDER BY child`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: querying event_children: %w", err)
	}
	defer rows.Close()

	out := map[int64][]string{}
	for rows.Next() {
		var eventID int64
		var child string
		if err := rows.Scan(&eventID, &child); err != nil {
			return nil, fmt.Errorf("store: scanning event_children: %w", err)
		}
		out[eventID] = append(out[eventID], child)
	}
	return out, rows.Err()
}

// ReextractFilter selects which already-extracted messages a reextract run
// should cover. The everyday pending queue (PendingMessages) is untouched by
// this: pending messages belong to `extract`, and including them here would
// double-extract them.
type ReextractFilter struct {
	ExtractVer int       // usually extract.ExtractVer; messages already at this version or newer are skipped
	Force      bool      // ignore ExtractVer entirely (e.g. re-run with a stronger model)
	Since      time.Time // zero = no lower bound; compares messages.sent_at
	WilmaIDs   []int64   // non-empty = only these ids
	Limit      int
}

// ReextractMessages returns messages eligible for another extraction pass,
// oldest first. Reuses PendingMessage: a message is a message, whether it's
// being extracted for the first time or the third.
func (s *Store) ReextractMessages(f ReextractFilter) ([]PendingMessage, error) {
	query := `
		SELECT wilma_id, content_hash, subject, COALESCE(body_text, ''), sent_at, sent_at_raw, url
		FROM messages
		WHERE extract_state IN ('done', 'failed')
		  AND COALESCE(body_text, '') <> ''`
	var args []any

	if !f.Force {
		// extract_ver IS NULL on a message that has only ever failed —
		// bare "< ?" against NULL is NULL (i.e. false), which would
		// silently exclude it, so the IS NULL branch matters.
		query += ` AND (extract_ver IS NULL OR extract_ver < ?)`
		args = append(args, f.ExtractVer)
	}
	if !f.Since.IsZero() {
		query += ` AND sent_at IS NOT NULL AND sent_at >= ?`
		args = append(args, f.Since.UTC().Format(time.RFC3339))
	}
	if len(f.WilmaIDs) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(f.WilmaIDs)), ",")
		query += ` AND wilma_id IN (` + placeholders + `)`
		for _, id := range f.WilmaIDs {
			args = append(args, id)
		}
	}
	query += ` ORDER BY wilma_id ASC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: querying reextract candidates: %w", err)
	}
	defer rows.Close()

	var out []PendingMessage
	for rows.Next() {
		var pm PendingMessage
		var sentAt sql.NullString
		if err := rows.Scan(&pm.WilmaID, &pm.ContentHash, &pm.Subject, &pm.BodyText, &sentAt, &pm.SentAtRaw, &pm.URL); err != nil {
			return nil, fmt.Errorf("store: scanning reextract candidate: %w", err)
		}
		if sentAt.Valid {
			if t, err := time.Parse(time.RFC3339, sentAt.String); err == nil {
				pm.SentAt = t
			}
		}
		out = append(out, pm)
	}
	return out, rows.Err()
}

// SetHighWaterMark records the highest wilma_id ingested for a role,
// never lowering an existing value (so an out-of-order or partial ingest
// can't roll the mark backward).
func (s *Store) SetHighWaterMark(role string, wilmaID int64) error {
	_, err := s.db.Exec(`
		INSERT INTO sync_state (role, last_id, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (role) DO UPDATE SET
			last_id = MAX(last_id, excluded.last_id),
			updated_at = CASE WHEN excluded.last_id > last_id THEN excluded.updated_at ELSE updated_at END`,
		role, wilmaID, nowRFC3339(),
	)
	if err != nil {
		return fmt.Errorf("store: setting high-water mark for %q: %w", role, err)
	}
	return nil
}

// HighWaterMark returns the stored last_id for a role, or ok=false if
// nothing has been recorded for it yet.
func (s *Store) HighWaterMark(role string) (id int64, ok bool, err error) {
	err = s.db.QueryRow(`SELECT last_id FROM sync_state WHERE role = ?`, role).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: reading high-water mark for %q: %w", role, err)
	}
	return id, true, nil
}

// AllHighWaterMarks returns every role's stored last_id.
func (s *Store) AllHighWaterMarks() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT role, last_id FROM sync_state ORDER BY role`)
	if err != nil {
		return nil, fmt.Errorf("store: querying sync_state: %w", err)
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var role string
		var id int64
		if err := rows.Scan(&role, &id); err != nil {
			return nil, fmt.Errorf("store: scanning sync_state: %w", err)
		}
		out[role] = id
	}
	return out, rows.Err()
}

// MarkPolled records that role was reached by a poll pass at the current
// time, whether or not anything new was found. Deliberately independent of
// sync_state/SetHighWaterMark — see the poll_state comment in schema.go for
// why the two must not share a table.
func (s *Store) MarkPolled(role string) error {
	_, err := s.db.Exec(`
		INSERT INTO poll_state (role, last_polled_at) VALUES (?, ?)
		ON CONFLICT (role) DO UPDATE SET last_polled_at = excluded.last_polled_at`,
		role, nowRFC3339(),
	)
	if err != nil {
		return fmt.Errorf("store: recording poll time for %q: %w", role, err)
	}
	return nil
}

// LastPolledAt returns when role was last reached by a poll pass, or
// ok=false if it never has been.
func (s *Store) LastPolledAt(role string) (t time.Time, ok bool, err error) {
	var raw string
	err = s.db.QueryRow(`SELECT last_polled_at FROM poll_state WHERE role = ?`, role).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: reading poll time for %q: %w", role, err)
	}
	t, err = time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: parsing poll time for %q: %w", role, err)
	}
	return t, true, nil
}

// AllLastPolledAt returns every role's stored last_polled_at.
func (s *Store) AllLastPolledAt() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT role, last_polled_at FROM poll_state ORDER BY role`)
	if err != nil {
		return nil, fmt.Errorf("store: querying poll_state: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var role, raw string
		if err := rows.Scan(&role, &raw); err != nil {
			return nil, fmt.Errorf("store: scanning poll_state: %w", err)
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, fmt.Errorf("store: parsing poll time for %q: %w", role, err)
		}
		out[role] = t
	}
	return out, rows.Err()
}
