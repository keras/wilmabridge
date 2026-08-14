package store

import (
	"errors"
	"path/filepath"
	"strconv"
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
			WilmaID: 42, Kind: "event", Title: "Uinti", DateRaw: "10.10.", WeekdayClaim: "la",
			Items: []string{"uimahousut", "pyyhe"}, Recurrence: &extract.Recurrence{Freq: "weekly", Count: 4},
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

	if err := s.SaveEvents(42, hash, events); err != nil {
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
	if len(uinti.Items) != 2 || uinti.Items[0] != "uimahousut" {
		t.Errorf("items = %v", uinti.Items)
	}
	if uinti.Recurrence == nil || uinti.Recurrence.Freq != "weekly" || uinti.Recurrence.Count != 4 {
		t.Errorf("recurrence = %+v", uinti.Recurrence)
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
	if fyi.Recurrence != nil {
		t.Errorf("recurrence should be nil, got %+v", fyi.Recurrence)
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

	ex := gemini.RawExchange{
		Request: []byte(`{"model":"gemini-3.5-flash-lite"}`), Response: []byte(`{"status":"completed"}`),
		StatusCode: 200, Attempts: 1,
	}
	if err := s.SaveExtraction(9, hash, "gemini-3.5-flash-lite", ex, nil); err != nil {
		t.Fatalf("SaveExtraction (success): %v", err)
	}
	if err := s.SaveExtraction(9, hash, "gemini-3.5-flash-lite", gemini.RawExchange{StatusCode: 429, Attempts: 3}, errors.New("quota exceeded")); err != nil {
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
