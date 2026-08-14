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
	return s, nil
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

// SaveExtraction records one Gemini call attempt — successful or not — for
// audit and later replay. Uses gemini.RawExchange, which ExtractMessage
// already computes; before this package existed, cmdExtract discarded it.
func (s *Store) SaveExtraction(wilmaID int64, contentHash, model string, ex gemini.RawExchange, callErr error) error {
	var errMsg any
	if callErr != nil {
		errMsg = callErr.Error()
	}
	_, err := s.db.Exec(`
		INSERT INTO extractions
			(wilma_id, content_hash, model, request, response, status_code, http_attempts, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		wilmaID, contentHash, model, string(ex.Request), nullString(string(ex.Response)),
		nullInt64(int64(ex.StatusCode)), nullInt64(int64(ex.Attempts)), errMsg, nowRFC3339(),
	)
	if err != nil {
		return fmt.Errorf("store: saving extraction for message %d: %w", wilmaID, err)
	}
	return nil
}

// SaveEvents persists the events found in one message. Call once per
// successful extraction; MarkExtractDone should follow.
func (s *Store) SaveEvents(wilmaID int64, contentHash string, events []extract.Event) error {
	now := nowRFC3339()
	for _, ev := range events {
		itemsJSON, err := jsonOrNil(ev.Items, len(ev.Items) == 0)
		if err != nil {
			return fmt.Errorf("store: encoding items for message %d: %w", wilmaID, err)
		}
		recJSON, err := jsonOrNil(ev.Recurrence, ev.Recurrence == nil)
		if err != nil {
			return fmt.Errorf("store: encoding recurrence for message %d: %w", wilmaID, err)
		}
		reasonsJSON, err := jsonOrNil(ev.ReviewReasons, len(ev.ReviewReasons) == 0)
		if err != nil {
			return fmt.Errorf("store: encoding review_reasons for message %d: %w", wilmaID, err)
		}
		var weekdayOK any
		if ev.WeekdayOK != nil {
			weekdayOK = boolToInt(*ev.WeekdayOK)
		}

		if _, err := s.db.Exec(`
			INSERT INTO events
				(wilma_id, content_hash, kind, title, detail, date_raw, weekday_claim, time,
				 location, items, link, recurrence, model_confidence, quote, resolved_date,
				 weekday_ok, needs_review, review_reasons, extract_ver, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			wilmaID, contentHash, ev.Kind, ev.Title, nullString(ev.Detail), ev.DateRaw,
			nullString(ev.WeekdayClaim), nullString(ev.Time), nullString(ev.Location),
			itemsJSON, nullString(ev.Link), recJSON, ev.ModelConfidence, ev.Quote,
			nullString(ev.ResolvedDate), weekdayOK, boolToInt(ev.NeedsReview), reasonsJSON,
			ev.ExtractVer, now,
		); err != nil {
			return fmt.Errorf("store: saving event %q for message %d: %w", ev.Title, wilmaID, err)
		}
	}
	return nil
}

// MarkExtractDone transitions a message to 'done' after its events (zero or
// more) have been saved successfully.
func (s *Store) MarkExtractDone(wilmaID int64, contentHash string, extractVer int) error {
	_, err := s.db.Exec(`
		UPDATE messages SET extract_state = 'done', extract_ver = ?, last_error = NULL
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

// ReviewRow is a needs_review event annotated with which children it
// concerns (via message_children — events themselves carry no child, since
// extraction runs once per message regardless of recipients).
type ReviewRow struct {
	extract.Event
	ContentHash string   `json:"-"` // internal join key; not part of the public NDJSON shape
	Children    []string `json:"children"`
}

// NeedsReview returns every event flagged needs_review, most recent first.
func (s *Store) NeedsReview() ([]ReviewRow, error) {
	rows, err := s.db.Query(`
		SELECT e.id, e.wilma_id, e.content_hash, e.kind, e.title, e.detail, e.date_raw,
		       e.weekday_claim, e.time, e.location, e.items, e.link, e.recurrence,
		       e.model_confidence, e.quote, e.resolved_date, e.weekday_ok, e.review_reasons,
		       e.extract_ver, m.subject, m.url
		FROM events e
		JOIN messages m ON m.wilma_id = e.wilma_id AND m.content_hash = e.content_hash
		WHERE e.needs_review = 1
		ORDER BY e.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: querying needs_review events: %w", err)
	}
	defer rows.Close()

	var out []ReviewRow
	for rows.Next() {
		var r ReviewRow
		var id int64
		var detail, weekdayClaim, evTime, location, itemsJSON, link, recJSON, resolvedDate, reasonsJSON sql.NullString
		var weekdayOK sql.NullInt64
		var confidence sql.NullFloat64
		if err := rows.Scan(
			&id, &r.WilmaID, &r.ContentHash, &r.Kind, &r.Title, &detail, &r.DateRaw,
			&weekdayClaim, &evTime, &location, &itemsJSON, &link, &recJSON,
			&confidence, &r.Quote, &resolvedDate, &weekdayOK, &reasonsJSON,
			&r.ExtractVer, &r.MessageSubject, &r.MessageURL,
		); err != nil {
			return nil, fmt.Errorf("store: scanning needs_review event: %w", err)
		}
		r.Detail = detail.String
		r.WeekdayClaim = weekdayClaim.String
		r.Time = evTime.String
		r.Location = location.String
		r.Link = link.String
		r.ResolvedDate = resolvedDate.String
		r.ModelConfidence = confidence.Float64
		r.NeedsReview = true

		if itemsJSON.Valid {
			if err := json.Unmarshal([]byte(itemsJSON.String), &r.Items); err != nil {
				return nil, fmt.Errorf("store: decoding items for event %d: %w", id, err)
			}
		}
		if recJSON.Valid {
			r.Recurrence = &extract.Recurrence{}
			if err := json.Unmarshal([]byte(recJSON.String), r.Recurrence); err != nil {
				return nil, fmt.Errorf("store: decoding recurrence for event %d: %w", id, err)
			}
		}
		if reasonsJSON.Valid {
			if err := json.Unmarshal([]byte(reasonsJSON.String), &r.ReviewReasons); err != nil {
				return nil, fmt.Errorf("store: decoding review_reasons for event %d: %w", id, err)
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

	children, err := s.childrenByMessage()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Children = children[messageKey(out[i].WilmaID, out[i].ContentHash)]
	}
	return out, nil
}

func messageKey(wilmaID int64, contentHash string) string {
	return fmt.Sprintf("%d|%s", wilmaID, contentHash)
}

func (s *Store) childrenByMessage() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT wilma_id, content_hash, child FROM message_children ORDER BY child`)
	if err != nil {
		return nil, fmt.Errorf("store: querying message_children: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var wilmaID int64
		var hash, child string
		if err := rows.Scan(&wilmaID, &hash, &child); err != nil {
			return nil, fmt.Errorf("store: scanning message_children: %w", err)
		}
		key := messageKey(wilmaID, hash)
		out[key] = append(out[key], child)
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
