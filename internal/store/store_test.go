package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wilmabridge/internal/extract"
	"wilmabridge/internal/gemini"
	"wilmabridge/internal/wilma"
)

// openTestStore opens a fresh SQLite file under t.TempDir() — a real file
// rather than an in-memory DSN, since the modernc.org/sqlite driver's
// in-memory DSN quirks weren't worth relying on for tests that must be
// trustworthy.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wilma.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testMsg(id int64, child, role, subject, body string, sentAt time.Time) wilma.Message {
	return wilma.Message{
		ID: id, Child: child, Role: role, Subject: subject, BodyText: body,
		BodyHTML: "<p>" + body + "</p>", SentAt: sentAt, SentAtRaw: sentAt.Format("2006-01-02 15:04"),
		URL: "https://espoo.inschool.fi" + role + "/messages/" + strconv.FormatInt(id, 10), Unread: true,
	}
}

// testRun starts a run for tests that don't care about its particulars.
func testRun(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.StartRun("test-model", 1, "test")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return id
}

func TestOpen_MigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wilma.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (re-migrate existing db): %v", err)
	}
	s2.Close()
}

// legacySchemaSQL is schemaSQL as it existed before extraction_runs,
// run_messages, event_children, and events/extractions.run_id and
// events.audience were added — i.e. exactly what a real pre-existing
// wilmabridge database looks like. Kept as a literal copy (not derived from
// the current schema.go) so this test actually exercises the ALTER TABLE /
// backfill path in migrate() rather than a no-op CREATE TABLE IF NOT EXISTS.
const legacySchemaSQL = `
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

CREATE TABLE IF NOT EXISTS poll_state (
  role           TEXT PRIMARY KEY,
  last_polled_at TEXT NOT NULL
);
`

// TestOpen_MigratesLegacyDatabase builds a database on the pre-run-tracking
// schema, seeds it exactly the way real pre-existing data would look
// (a message, two children, a needs_review event, an extraction audit
// row), then Opens it with today's code and checks the migration backfill
// reproduces the old implicit behavior: one synthetic run, everything
// attached to it, audience defaulted to "child", and NeedsReview()'s output
// unchanged in shape from before the migration.
func TestOpen_MigratesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wilma.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening raw legacy db: %v", err)
	}
	if _, err := raw.Exec(legacySchemaSQL); err != nil {
		t.Fatalf("creating legacy schema: %v", err)
	}
	sentAt := time.Date(2026, 8, 12, 8, 37, 0, 0, time.UTC)
	if _, err := raw.Exec(`
		INSERT INTO messages (wilma_id, content_hash, subject, sent_at, sent_at_raw, body_text, url, fetched_at, extract_state, extract_ver)
		VALUES (1, 'h1', 'Retki', ?, ?, 'body', 'https://x/1', ?, 'done', 1)`,
		sentAt.Format(time.RFC3339), sentAt.Format("2006-01-02 15:04"), sentAt.Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seeding legacy message: %v", err)
	}
	for i, child := range []string{"Ella", "Jooa"} {
		if _, err := raw.Exec(`
			INSERT INTO message_children (wilma_id, content_hash, child, role_prefix, was_unread) VALUES (1, 'h1', ?, ?, 0)`,
			child, "/!"+strconv.Itoa(i+1),
		); err != nil {
			t.Fatalf("seeding legacy message_children: %v", err)
		}
	}
	if _, err := raw.Exec(`
		INSERT INTO events (wilma_id, content_hash, kind, title, date_raw, quote, needs_review, review_reasons, extract_ver, created_at)
		VALUES (1, 'h1', 'event', 'Retkipäivä', '9.5.', 'q', 1, '["weekend"]', 1, ?)`,
		sentAt.Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seeding legacy event: %v", err)
	}
	if _, err := raw.Exec(`
		INSERT INTO extractions (wilma_id, content_hash, model, request, created_at) VALUES (1, 'h1', 'old-model', '{}', ?)`,
		sentAt.Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seeding legacy extraction: %v", err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate legacy db): %v", err)
	}
	defer s.Close()

	var runCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM extraction_runs`).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Errorf("extraction_runs count = %d, want 1 (the backfill run)", runCount)
	}

	var orphanEvents, orphanExtractions int
	s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE run_id IS NULL`).Scan(&orphanEvents)
	s.db.QueryRow(`SELECT COUNT(*) FROM extractions WHERE run_id IS NULL`).Scan(&orphanExtractions)
	if orphanEvents != 0 || orphanExtractions != 0 {
		t.Errorf("orphaned rows after migration: events=%d extractions=%d, want 0/0", orphanEvents, orphanExtractions)
	}

	var audience string
	if err := s.db.QueryRow(`SELECT audience FROM events WHERE wilma_id = 1`).Scan(&audience); err != nil {
		t.Fatal(err)
	}
	if audience != "child" {
		t.Errorf("legacy event audience = %q, want %q", audience, "child")
	}

	var coverage int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM run_messages WHERE wilma_id = 1`).Scan(&coverage); err != nil {
		t.Fatal(err)
	}
	if coverage != 1 {
		t.Errorf("run_messages coverage rows for message 1 = %d, want 1", coverage)
	}

	rows, err := s.NeedsReview()
	if err != nil {
		t.Fatalf("NeedsReview after migration: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("NeedsReview after migration = %d rows, want 1", len(rows))
	}
	if len(rows[0].Children) != 2 {
		t.Errorf("migrated event's children = %v, want both Ella and Jooa (matching the old blanket message_children join)", rows[0].Children)
	}

	// Idempotency: opening the now-migrated db again must not create a
	// second backfill run or duplicate any data.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	var runCount2 int
	s2.db.QueryRow(`SELECT COUNT(*) FROM extraction_runs`).Scan(&runCount2)
	if runCount2 != 1 {
		t.Errorf("extraction_runs count after second Open = %d, want still 1", runCount2)
	}
}

func TestIngestMessage_DedupAcrossChildren(t *testing.T) {
	s := openTestStore(t)
	sentAt := time.Date(2026, 8, 12, 8, 37, 0, 0, time.UTC)

	// Same wilma_id, same content, arriving via two different children's
	// role feeds -- the exact real-world case (message 19824275 seen live
	// in both kids' inboxes with identical subject/sender/timestamp).
	msgA := testMsg(19824275, "Ella Taina", "/!04252751", "Kouluterveydenhoitajan info", "body", sentAt)
	msgB := testMsg(19824275, "Jooa Taina", "/!04292956", "Kouluterveydenhoitajan info", "body", sentAt)

	isNewA, err := s.IngestMessage(msgA)
	if err != nil {
		t.Fatalf("ingest A: %v", err)
	}
	if !isNewA {
		t.Error("first ingest should report isNew=true")
	}

	isNewB, err := s.IngestMessage(msgB)
	if err != nil {
		t.Fatalf("ingest B: %v", err)
	}
	if isNewB {
		t.Error("second ingest of identical content should report isNew=false")
	}

	var messageCount, childCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE wilma_id = 19824275`).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message_children WHERE wilma_id = 19824275`).Scan(&childCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 1 {
		t.Errorf("messages rows = %d, want 1 (one extraction, not two)", messageCount)
	}
	if childCount != 2 {
		t.Errorf("message_children rows = %d, want 2 (both kids linked)", childCount)
	}
}

func TestIngestMessage_DifferentContentSameID_StaysSeparate(t *testing.T) {
	s := openTestStore(t)
	sentAt := time.Date(2026, 8, 12, 8, 37, 0, 0, time.UTC)

	msgV1 := testMsg(500, "Ella", "/!1", "Subject", "original body", sentAt)
	msgV2 := testMsg(500, "Ella", "/!1", "Subject", "edited body", sentAt)

	if _, err := s.IngestMessage(msgV1); err != nil {
		t.Fatal(err)
	}
	isNew, err := s.IngestMessage(msgV2)
	if err != nil {
		t.Fatal(err)
	}
	if !isNew {
		t.Error("genuinely different content under the same wilma_id should be a new row")
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE wilma_id = 500`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("messages rows = %d, want 2 (different content_hash, kept separate)", count)
	}
}

func TestIngestMessage_EmptyBodySkipped(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(1, "Ella", "/!1", "No body", "", time.Now())
	msg.BodyText = ""

	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT extract_state FROM messages WHERE wilma_id = 1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" {
		t.Errorf("extract_state = %q, want skipped", state)
	}

	pending, err := s.PendingMessages(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("skipped message should not appear in PendingMessages: %+v", pending)
	}
}

func TestPendingMessages_LimitAndOrder(t *testing.T) {
	s := openTestStore(t)
	for i := int64(1); i <= 5; i++ {
		msg := testMsg(i, "Ella", "/!1", "s", "body", time.Now())
		if _, err := s.IngestMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingMessages(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("got %d pending, want 3 (limit respected)", len(pending))
	}
	if pending[0].WilmaID != 1 || pending[1].WilmaID != 2 || pending[2].WilmaID != 3 {
		t.Errorf("order = %v, want ascending by wilma_id", pending)
	}
}

func TestSaveEvents_RoundTripsAllFieldTypes(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(42, "Jooa", "/!2", "Neljä uintivuoroa", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	weekdayOKTrue := true
	weekdayOKFalse := false
	events := []extract.Event{
		{
			// Note this represents ONE occurrence of what would have been a
			// recurring candidate before extraction -- BuildEvent (tested
			// separately in internal/extract) is what expands a Candidate's
			// Recurrence into several of these; SaveEvents just persists
			// whatever Event slice it's handed, one at a time.
			WilmaID: 42, Kind: "event", Title: "Uinti", DateRaw: "10.10.", WeekdayClaim: "la",
			ResolvedDate: "2026-10-10", WeekdayOK: &weekdayOKTrue, NeedsReview: true,
			ReviewReasons: []string{"2026-10-10 falls on a weekend (lauantai)"},
			Quote:         "quote1", ExtractVer: 1,
		},
		{
			WilmaID: 42, Kind: "exam", Title: "Koe", DateRaw: "5.3.", WeekdayClaim: "to",
			ResolvedDate: "2026-03-05", WeekdayOK: &weekdayOKFalse, NeedsReview: false,
			Quote: "quote2", ExtractVer: 1,
		},
		{
			// No date at all: NeedsReview true, ResolvedDate/WeekdayOK left zero/nil.
			WilmaID: 42, Kind: "info", Title: "FYI", DateRaw: "ensi viikolla",
			NeedsReview: true, ReviewReasons: []string{`could not parse date "ensi viikolla"`},
			Quote: "quote3", ExtractVer: 1,
		},
	}

	runID := testRun(t, s)
	if err := s.SaveEvents(runID, 42, hash, events); err != nil {
		t.Fatalf("SaveEvents: %v", err)
	}
	if err := s.MarkExtractDone(42, hash, 1); err != nil {
		t.Fatalf("MarkExtractDone: %v", err)
	}

	rows, err := s.NeedsReview()
	if err != nil {
		t.Fatalf("NeedsReview: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d needs_review rows, want 2", len(rows))
	}

	byTitle := map[string]ReviewRow{}
	for _, r := range rows {
		byTitle[r.Title] = r
	}

	uinti, ok := byTitle["Uinti"]
	if !ok {
		t.Fatal("missing Uinti event")
	}
	if uinti.WeekdayOK == nil || !*uinti.WeekdayOK {
		t.Errorf("weekday_ok = %v, want true", uinti.WeekdayOK)
	}
	if len(uinti.ReviewReasons) != 1 {
		t.Errorf("review_reasons = %v", uinti.ReviewReasons)
	}
	if len(uinti.Children) != 1 || uinti.Children[0] != "Jooa" {
		t.Errorf("children = %v, want [Jooa]", uinti.Children)
	}

	fyi, ok := byTitle["FYI"]
	if !ok {
		t.Fatal("missing FYI event")
	}
	if fyi.ResolvedDate != "" {
		t.Errorf("unparseable date should leave resolved_date empty, got %q", fyi.ResolvedDate)
	}
	if fyi.WeekdayOK != nil {
		t.Errorf("weekday_ok should be nil when there was nothing to check, got %v", *fyi.WeekdayOK)
	}

	// The exam (NeedsReview=false) must not appear in the review queue at all.
	if _, present := byTitle["Koe"]; present {
		t.Error("non-review event should not appear in NeedsReview()")
	}

	var state string
	var ver int
	if err := s.db.QueryRow(`SELECT extract_state, extract_ver FROM messages WHERE wilma_id = 42`).Scan(&state, &ver); err != nil {
		t.Fatal(err)
	}
	if state != "done" || ver != 1 {
		t.Errorf("state=%q ver=%d, want done/1", state, ver)
	}
}

func TestStartRun_FinishRun(t *testing.T) {
	s := openTestStore(t)

	runID, err := s.StartRun("gemini-3.5-flash-lite", 2, "pending queue")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	runs, err := s.Runs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("Runs() = %d, want 1", len(runs))
	}
	if runs[0].ID != runID || runs[0].Model != "gemini-3.5-flash-lite" || runs[0].ExtractVer != 2 {
		t.Errorf("run = %+v", runs[0])
	}
	if !runs[0].FinishedAt.IsZero() {
		t.Error("FinishedAt should be zero before FinishRun")
	}

	if err := s.FinishRun(runID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	runs, err = s.Runs(10)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].FinishedAt.IsZero() {
		t.Error("FinishedAt should be set after FinishRun")
	}
}

func TestRun_MarshalJSONOmitsZeroFinishedAt(t *testing.T) {
	r := Run{ID: 1, Model: "m", ExtractVer: 1, StartedAt: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"finished_at"`) {
		t.Errorf("expected finished_at to be omitted for a zero time (struct-typed omitempty is a no-op in encoding/json), got: %s", b)
	}

	r.FinishedAt = time.Date(2026, 8, 14, 1, 0, 0, 0, time.UTC)
	b, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"finished_at":"2026-08-14T01:00:00Z"`) {
		t.Errorf("expected finished_at to be present once set, got: %s", b)
	}
}

func TestSaveEvents_AudienceChildFansOutToAllChildren(t *testing.T) {
	s := openTestStore(t)
	sentAt := time.Date(2026, 8, 12, 8, 37, 0, 0, time.UTC)
	msgA := testMsg(1, "Ella", "/!1", "Koko koulun tiedote", "body", sentAt)
	msgB := testMsg(1, "Jooa", "/!2", "Koko koulun tiedote", "body", sentAt)
	if _, err := s.IngestMessage(msgA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestMessage(msgB); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msgA.Subject, msgA.SentAtRaw, msgA.BodyHTML)

	runID := testRun(t, s)
	events := []extract.Event{{WilmaID: 1, Kind: "event", Title: "Retki", DateRaw: "9.5.", Quote: "q", Audience: extract.AudienceChild, ExtractVer: 2}}
	if err := s.SaveEvents(runID, 1, hash, events); err != nil {
		t.Fatalf("SaveEvents: %v", err)
	}

	rows, err := s.LatestEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("LatestEvents = %d rows, want 1", len(rows))
	}
	if len(rows[0].Children) != 2 {
		t.Errorf("children = %v, want both Ella and Jooa (whole-school fan-out)", rows[0].Children)
	}
}

func TestSaveEvents_AudienceGuardiansHasNoChildren(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(1, "Ella", "/!1", "Vanhempainilta", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	runID := testRun(t, s)
	events := []extract.Event{{WilmaID: 1, Kind: "event", Title: "Vanhempainilta", DateRaw: "26.8.", Quote: "q", Audience: extract.AudienceGuardians, ExtractVer: 2}}
	if err := s.SaveEvents(runID, 1, hash, events); err != nil {
		t.Fatalf("SaveEvents: %v", err)
	}

	rows, err := s.LatestEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("LatestEvents = %d rows, want 1", len(rows))
	}
	if rows[0].Children == nil || len(rows[0].Children) != 0 {
		t.Errorf("children = %#v, want a non-nil empty slice (guardians-only, deliberately zero)", rows[0].Children)
	}
	if rows[0].Audience != extract.AudienceGuardians {
		t.Errorf("audience = %q, want %q", rows[0].Audience, extract.AudienceGuardians)
	}
}

func TestLatestEvents_LatestRunWins(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(1, "Ella", "/!1", "s", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	run1 := testRun(t, s)
	if err := s.SaveEvents(run1, 1, hash, []extract.Event{
		{WilmaID: 1, Kind: "event", Title: "Old A", DateRaw: "1.1.", Quote: "q", Audience: extract.AudienceChild, ExtractVer: 1},
		{WilmaID: 1, Kind: "event", Title: "Old B", DateRaw: "2.1.", Quote: "q", Audience: extract.AudienceChild, ExtractVer: 1},
	}); err != nil {
		t.Fatal(err)
	}

	run2 := testRun(t, s)
	if err := s.SaveEvents(run2, 1, hash, []extract.Event{
		{WilmaID: 1, Kind: "event", Title: "New", DateRaw: "3.1.", Quote: "q", Audience: extract.AudienceChild, ExtractVer: 2},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LatestEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Title != "New" {
		t.Fatalf("LatestEvents = %+v, want only run2's single event", rows)
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total events rows = %d, want 3 (old run's rows are neither deleted nor mutated)", total)
	}
	var oldTitle string
	if err := s.db.QueryRow(`SELECT title FROM events WHERE title = 'Old A'`).Scan(&oldTitle); err != nil {
		t.Errorf("run1's event should still exist unmutated: %v", err)
	}
}

func TestLatestEvents_RerunWithZeroEventsSupersedesOldOnes(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(1, "Ella", "/!1", "s", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	run1 := testRun(t, s)
	if err := s.SaveEvents(run1, 1, hash, []extract.Event{
		{WilmaID: 1, Kind: "event", Title: "Stale", DateRaw: "1.1.", Quote: "q", Audience: extract.AudienceChild, ExtractVer: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// A re-run correctly finds nothing this time (e.g. the model changed
	// its mind, or the message genuinely has no actionable date under a
	// revised prompt). SaveEvents must still be called with an empty slice
	// -- this is the case run_messages exists to handle correctly.
	run2 := testRun(t, s)
	if err := s.SaveEvents(run2, 1, hash, nil); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LatestEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("LatestEvents = %+v, want none (the zero-event rerun must supersede the stale event, not fall back to it)", rows)
	}
}

func TestLatestEvents_FailedRerunKeepsPreviousGeneration(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(1, "Ella", "/!1", "s", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	run1 := testRun(t, s)
	if err := s.SaveEvents(run1, 1, hash, []extract.Event{
		{WilmaID: 1, Kind: "event", Title: "Still current", DateRaw: "1.1.", Quote: "q", Audience: extract.AudienceChild, ExtractVer: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// A second run starts but the extraction call itself fails: only
	// SaveExtraction (the audit row) is recorded, never SaveEvents, so no
	// run_messages coverage row exists for run2 either.
	run2 := testRun(t, s)
	if err := s.SaveExtraction(run2, 1, hash, "gemini-3.5-flash-lite", gemini.RawExchange{StatusCode: 500}, errors.New("server error")); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LatestEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Title != "Still current" {
		t.Fatalf("LatestEvents = %+v, want run1's event still standing (a failed rerun must not blank out the previous generation)", rows)
	}
}

func TestLatestEvents_DateRangeFilter(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(1, "Ella", "/!1", "s", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	run := testRun(t, s)
	if err := s.SaveEvents(run, 1, hash, []extract.Event{
		{WilmaID: 1, Kind: "event", Title: "Before", DateRaw: "x", Quote: "q", Audience: extract.AudienceChild, ResolvedDate: "2026-07-18", ExtractVer: 1},
		{WilmaID: 1, Kind: "event", Title: "In range, later", DateRaw: "x", Quote: "q", Audience: extract.AudienceChild, ResolvedDate: "2026-07-19", Time: "14:00", ExtractVer: 1},
		{WilmaID: 1, Kind: "event", Title: "In range, earlier", DateRaw: "x", Quote: "q", Audience: extract.AudienceChild, ResolvedDate: "2026-07-19", Time: "09:00", ExtractVer: 1},
		{WilmaID: 1, Kind: "event", Title: "After", DateRaw: "x", Quote: "q", Audience: extract.AudienceChild, ResolvedDate: "2026-07-20", ExtractVer: 1},
		{WilmaID: 1, Kind: "event", Title: "Unresolved", DateRaw: "x", Quote: "q", Audience: extract.AudienceChild, ResolvedDate: "", ExtractVer: 1},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LatestEvents(EventFilter{DateFrom: "2026-07-19", DateTo: "2026-07-19"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("LatestEvents = %+v, want exactly the 2 events resolved to 2026-07-19", rows)
	}
	if rows[0].Title != "In range, earlier" || rows[1].Title != "In range, later" {
		t.Errorf("got titles [%s, %s], want chronological order by time within the same date", rows[0].Title, rows[1].Title)
	}

	rows, err = s.LatestEvents(EventFilter{DateFrom: "2026-07-19", DateTo: "2026-07-20"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("LatestEvents = %+v, want the 2 events on 2026-07-19 plus the 1 on 2026-07-20", rows)
	}

	rows, err = s.LatestEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Errorf("LatestEvents with no date filter = %d rows, want all 5 including the unresolved one", len(rows))
	}
}

func TestNeedsReview_OnlyLatestRun(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(1, "Ella", "/!1", "s", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	run1 := testRun(t, s)
	if err := s.SaveEvents(run1, 1, hash, []extract.Event{
		{WilmaID: 1, Kind: "event", Title: "Flagged old", DateRaw: "1.1.", Quote: "q", Audience: extract.AudienceChild, NeedsReview: true, ReviewReasons: []string{"x"}, ExtractVer: 1},
	}); err != nil {
		t.Fatal(err)
	}
	run2 := testRun(t, s)
	if err := s.SaveEvents(run2, 1, hash, []extract.Event{
		{WilmaID: 1, Kind: "event", Title: "Clean new", DateRaw: "2.1.", Quote: "q", Audience: extract.AudienceChild, NeedsReview: false, ExtractVer: 2},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.NeedsReview()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("NeedsReview() = %+v, want none: run2's event superseded run1's flagged one and isn't itself flagged", rows)
	}
}

func TestReextractMessages_VersionForceSinceAndIDs(t *testing.T) {
	s := openTestStore(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	msg1 := testMsg(1, "Ella", "/!1", "s1", "body", old) // done, ver 1 -> eligible by version
	if _, err := s.IngestMessage(msg1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkExtractDone(1, contentHash(msg1.Subject, msg1.SentAtRaw, msg1.BodyHTML), 1); err != nil {
		t.Fatal(err)
	}

	msg2 := testMsg(2, "Ella", "/!1", "s2", "body", recent) // done, ver 2 already -> not eligible unless forced
	if _, err := s.IngestMessage(msg2); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkExtractDone(2, contentHash(msg2.Subject, msg2.SentAtRaw, msg2.BodyHTML), 2); err != nil {
		t.Fatal(err)
	}

	msg3 := testMsg(3, "Ella", "/!1", "s3", "body", recent) // failed, extract_ver IS NULL -> must still be eligible
	if _, err := s.IngestMessage(msg3); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkExtractFailed(3, contentHash(msg3.Subject, msg3.SentAtRaw, msg3.BodyHTML), "boom", 1); err != nil {
		t.Fatal(err)
	}

	msg4 := testMsg(4, "Ella", "/!1", "s4", "body", old) // still pending -> must never be returned
	if _, err := s.IngestMessage(msg4); err != nil {
		t.Fatal(err)
	}

	// Default: version-gated, no Since bound.
	got, err := s.ReextractMessages(ReextractFilter{ExtractVer: 2, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := map[int64]bool{}
	for _, pm := range got {
		gotIDs[pm.WilmaID] = true
	}
	if !gotIDs[1] || !gotIDs[3] || gotIDs[2] || gotIDs[4] {
		t.Errorf("version-gated ReextractMessages ids = %v, want {1,3} (2 is current, 4 is still pending, 3's NULL extract_ver must count as stale)", gotIDs)
	}

	// Force: ignores version, includes the already-current message too.
	got, err = s.ReextractMessages(ReextractFilter{Force: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	gotIDs = map[int64]bool{}
	for _, pm := range got {
		gotIDs[pm.WilmaID] = true
	}
	if !gotIDs[1] || !gotIDs[2] || !gotIDs[3] {
		t.Errorf("forced ReextractMessages ids = %v, want {1,2,3}", gotIDs)
	}

	// Since: only messages sent on/after the bound.
	got, err = s.ReextractMessages(ReextractFilter{Force: true, Since: recent, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	gotIDs = map[int64]bool{}
	for _, pm := range got {
		gotIDs[pm.WilmaID] = true
	}
	if gotIDs[1] || !gotIDs[2] || !gotIDs[3] {
		t.Errorf("Since-filtered ReextractMessages ids = %v, want {2,3} only", gotIDs)
	}

	// WilmaIDs: an explicit id list.
	got, err = s.ReextractMessages(ReextractFilter{Force: true, WilmaIDs: []int64{2}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WilmaID != 2 {
		t.Errorf("WilmaIDs-filtered ReextractMessages = %+v, want just message 2", got)
	}
}

func TestMarkExtractFailed_CapsIntoFailedState(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(7, "Ella", "/!1", "s", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	const maxAttempts = 3
	for i := 0; i < maxAttempts-1; i++ {
		if err := s.MarkExtractFailed(7, hash, "quota exceeded", maxAttempts); err != nil {
			t.Fatal(err)
		}
		pending, err := s.PendingMessages(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 {
			t.Fatalf("attempt %d: message should still be pending (retry budget not spent), got %d pending", i+1, len(pending))
		}
	}

	// Final attempt spends the budget.
	if err := s.MarkExtractFailed(7, hash, "quota exceeded", maxAttempts); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingMessages(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("message should have moved to failed and left the pending queue, still pending: %+v", pending)
	}

	var state string
	var attempts int
	var lastErr string
	if err := s.db.QueryRow(`SELECT extract_state, attempts, last_error FROM messages WHERE wilma_id = 7`).Scan(&state, &attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || attempts != maxAttempts || lastErr != "quota exceeded" {
		t.Errorf("state=%q attempts=%d last_error=%q", state, attempts, lastErr)
	}
}

func TestSaveExtraction_RecordsAuditRow(t *testing.T) {
	s := openTestStore(t)
	msg := testMsg(9, "Ella", "/!1", "s", "body", time.Now())
	if _, err := s.IngestMessage(msg); err != nil {
		t.Fatal(err)
	}
	hash := contentHash(msg.Subject, msg.SentAtRaw, msg.BodyHTML)

	runID := testRun(t, s)
	ex := gemini.RawExchange{
		Request: []byte(`{"model":"gemini-3.5-flash-lite"}`), Response: []byte(`{"status":"completed"}`),
		StatusCode: 200, Attempts: 1,
	}
	if err := s.SaveExtraction(runID, 9, hash, "gemini-3.5-flash-lite", ex, nil); err != nil {
		t.Fatalf("SaveExtraction (success): %v", err)
	}
	if err := s.SaveExtraction(runID, 9, hash, "gemini-3.5-flash-lite", gemini.RawExchange{StatusCode: 429, Attempts: 3}, errors.New("quota exceeded")); err != nil {
		t.Fatalf("SaveExtraction (failure): %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM extractions WHERE wilma_id = 9`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("extractions rows = %d, want 2 (one per call attempt)", count)
	}
}

func TestHighWaterMark_MonotonicallyIncreases(t *testing.T) {
	s := openTestStore(t)
	role := "/!04252751"

	if _, ok, err := s.HighWaterMark(role); err != nil || ok {
		t.Fatalf("unset role should report ok=false, got ok=%v err=%v", ok, err)
	}

	if err := s.SetHighWaterMark(role, 100); err != nil {
		t.Fatal(err)
	}
	id, ok, err := s.HighWaterMark(role)
	if err != nil || !ok || id != 100 {
		t.Fatalf("id=%d ok=%v err=%v, want 100/true", id, ok, err)
	}

	// A lower value must not roll the mark backward.
	if err := s.SetHighWaterMark(role, 50); err != nil {
		t.Fatal(err)
	}
	id, _, _ = s.HighWaterMark(role)
	if id != 100 {
		t.Errorf("high-water mark regressed to %d after a lower SetHighWaterMark call", id)
	}

	if err := s.SetHighWaterMark(role, 150); err != nil {
		t.Fatal(err)
	}
	id, _, _ = s.HighWaterMark(role)
	if id != 150 {
		t.Errorf("id = %d, want 150 after a genuinely higher value", id)
	}

	all, err := s.AllHighWaterMarks()
	if err != nil {
		t.Fatal(err)
	}
	if all[role] != 150 {
		t.Errorf("AllHighWaterMarks()[%q] = %d, want 150", role, all[role])
	}
}

func TestMarkPolled_DoesNotCreateAPhantomHighWaterMark(t *testing.T) {
	s := openTestStore(t)
	role := "/!04252751"

	if err := s.MarkPolled(role); err != nil {
		t.Fatalf("MarkPolled: %v", err)
	}

	// A role that has been polled but has never had a message ingested
	// must still report ok=false -- see the poll_state comment in
	// schema.go for why last_id=0 would otherwise wrongly look like a
	// recorded mark and skip a role's bootstrap window entirely.
	if _, ok, err := s.HighWaterMark(role); err != nil || ok {
		t.Fatalf("HighWaterMark after MarkPolled only: ok=%v err=%v, want ok=false", ok, err)
	}
	all, err := s.AllHighWaterMarks()
	if err != nil {
		t.Fatal(err)
	}
	if _, present := all[role]; present {
		t.Errorf("AllHighWaterMarks should not include a role with no recorded mark, got %+v", all)
	}

	polledAt, ok, err := s.LastPolledAt(role)
	if err != nil || !ok || polledAt.IsZero() {
		t.Fatalf("LastPolledAt after MarkPolled: t=%v ok=%v err=%v, want a non-zero time and ok=true", polledAt, ok, err)
	}
}

func TestMarkPolled_DoesNotDisturbAnExistingMark(t *testing.T) {
	s := openTestStore(t)
	role := "/!04252751"

	if err := s.SetHighWaterMark(role, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPolled(role); err != nil {
		t.Fatalf("MarkPolled: %v", err)
	}

	id, ok, err := s.HighWaterMark(role)
	if err != nil || !ok || id != 100 {
		t.Fatalf("id=%d ok=%v err=%v, want 100/true unaffected by MarkPolled", id, ok, err)
	}
}

func TestLastPolledAt_UnsetRoleReportsNotOK(t *testing.T) {
	s := openTestStore(t)
	if _, ok, err := s.LastPolledAt("/!never-polled"); err != nil || ok {
		t.Fatalf("LastPolledAt for an unpolled role: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestAllLastPolledAt(t *testing.T) {
	s := openTestStore(t)
	if err := s.MarkPolled("/!111"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPolled("/!222"); err != nil {
		t.Fatal(err)
	}

	all, err := s.AllLastPolledAt()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all["/!111"].IsZero() || all["/!222"].IsZero() {
		t.Errorf("AllLastPolledAt() = %+v, want two non-zero entries", all)
	}
}
