package extract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"wilmabridge/internal/gemini"
	"wilmabridge/internal/wilma"
)

// fixtureServer serves the recorded response from internal/extract/testdata
// and returns its httptest URL, ready to hand to gemini.WithBaseURL. This
// is the "replay" path described in openclaw-integration.md's testing
// strategy: real captured Gemini output, replayed with no network access.
func fixtureServer(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	var f struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(f.Response)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func testMessage(t *testing.T, id int64, child, subject, body, sentAt string) wilma.Message {
	t.Helper()
	st := mustParseHelsinki(t, sentAt)
	return wilma.Message{
		ID: id, Child: child, Role: "/!04252751",
		Subject: subject, BodyText: body, SentAt: st,
		URL: "https://espoo.inschool.fi/!04252751/messages/" + strconv.FormatInt(id, 10),
	}
}

// TestExtractMessage_MonthlyLetter replays the real capture of the
// "Maaliskuun kuukausikirje" exchange and checks all four events come out
// with correctly resolved dates and clean weekday checks — this is the
// scenario from openclaw-integration.md's worked example 2.
func TestExtractMessage_MonthlyLetter(t *testing.T) {
	url := fixtureServer(t, "monthly_letter.json")
	client, err := gemini.NewClient("test-key", "gemini-3.5-flash-lite", gemini.WithBaseURL(url))
	if err != nil {
		t.Fatal(err)
	}
	msg := testMessage(t, 1, "Ella", "Maaliskuun kuukausikirje", "...", "2026-03-01")

	events, _, err := ExtractMessage(context.Background(), client, msg)
	if err != nil {
		t.Fatalf("ExtractMessage: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}

	want := []struct {
		kind, resolved string
		weekdayOK      bool
	}{
		{"event", "2026-03-04", true},
		{"exam", "2026-03-05", true},
		{"event", "2026-03-17", true},
		{"event", "2026-03-20", true},
	}
	for i, w := range want {
		ev := events[i]
		if ev.Kind != w.kind {
			t.Errorf("event[%d].Kind = %q, want %q", i, ev.Kind, w.kind)
		}
		if ev.ResolvedDate != w.resolved {
			t.Errorf("event[%d].ResolvedDate = %q, want %q", i, ev.ResolvedDate, w.resolved)
		}
		if ev.WeekdayOK == nil || *ev.WeekdayOK != w.weekdayOK {
			t.Errorf("event[%d].WeekdayOK = %v, want %v", i, ev.WeekdayOK, w.weekdayOK)
		}
		// This fixture was captured under the ver-1 schema, before the
		// "audience" field existed, so every replayed candidate is missing
		// it -- BuildEvent's never-silently-guess default kicks in and
		// flags exactly one reason per event. This is expected, not a
		// regression: see schema.go's ExtractVer doc comment. A genuinely
		// clean ver-2 response would set audience explicitly and need no
		// review at all.
		if !ev.NeedsReview || len(ev.ReviewReasons) != 1 {
			t.Errorf("event[%d] needs_review=%v reasons=%v, want exactly one reason (the ver-1 audience default)", i, ev.NeedsReview, ev.ReviewReasons)
		}
		if ev.Audience != AudienceChild {
			t.Errorf("event[%d].Audience = %q, want default %q", i, ev.Audience, AudienceChild)
		}
		if ev.Child != "Ella" || ev.WilmaID != 1 {
			t.Errorf("event[%d] source context = child=%q wilma_id=%d", i, ev.Child, ev.WilmaID)
		}
	}
}

// TestExtractMessage_Recurrence replays the real (post-fix) capture proving
// the schema's required:[freq,count] actually recovers the count — see
// schema.go's doc comment for the failure this guards against — and that
// BuildEvent expands the one {weekly,4} candidate into 4 independent Event
// rows, one per occurrence, rather than a single row carrying the
// recurrence hint. See validate.go's BuildEvent doc comment.
func TestExtractMessage_Recurrence(t *testing.T) {
	url := fixtureServer(t, "recurrence.json")
	client, err := gemini.NewClient("test-key", "gemini-3.5-flash-lite", gemini.WithBaseURL(url))
	if err != nil {
		t.Fatal(err)
	}
	msg := testMessage(t, 2, "Jooa", "Neljä uintivuoroa", "...", "2026-10-01")

	events, _, err := ExtractMessage(context.Background(), client, msg)
	if err != nil {
		t.Fatalf("ExtractMessage: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4 (one per occurrence): %+v", len(events), events)
	}

	// Weekly steps from 10.10.2026 (a Saturday, confirmed live) land on the
	// 17th, 24th, and 31st -- weekly preserves the weekday, so every single
	// occurrence genuinely falls on a Saturday.
	wantDates := []string{"2026-10-10", "2026-10-17", "2026-10-24", "2026-10-31"}
	for i, want := range wantDates {
		ev := events[i]
		if ev.Kind != "event" || ev.Title != "Uintivuoro" {
			t.Errorf("event[%d] = %+v", i, ev)
		}
		if ev.DateRaw != "10.10." {
			t.Errorf("event[%d].DateRaw = %q, want the same raw phrase on every occurrence (%q)", i, ev.DateRaw, "10.10.")
		}
		if ev.ResolvedDate != want {
			t.Errorf("event[%d].ResolvedDate = %q, want %q", i, ev.ResolvedDate, want)
		}
		// NeedsReview=true here is the weekend validator correctly firing on
		// every occurrence (real Saturdays, not a bug) -- this fixture also
		// predates "audience" (see TestExtractMessage_MonthlyLetter), which
		// adds a second reason on top.
		if !ev.NeedsReview {
			t.Errorf("event[%d]: expected needs_review=true (weekend + ver-1 audience default)", i)
		}
	}
}

func TestExtractMessage_Deadline(t *testing.T) {
	url := fixtureServer(t, "deadline.json")
	client, err := gemini.NewClient("test-key", "gemini-3.5-flash-lite", gemini.WithBaseURL(url))
	if err != nil {
		t.Fatal(err)
	}
	msg := testMessage(t, 3, "Ella", "STM kysely oppilaiden vanhemmille", "...", "2026-04-01")

	events, _, err := ExtractMessage(context.Background(), client, msg)
	if err != nil {
		t.Fatalf("ExtractMessage: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != "deadline" || events[0].ResolvedDate != "2026-04-14" {
		t.Errorf("event = %+v", events[0])
	}
}

// TestExtractMessage_NoDate is the hallucination guard: a message with no
// actionable date must come back as zero events, not an invented one.
func TestExtractMessage_NoDate(t *testing.T) {
	url := fixtureServer(t, "no_date.json")
	client, err := gemini.NewClient("test-key", "gemini-3.5-flash-lite", gemini.WithBaseURL(url))
	if err != nil {
		t.Fatal(err)
	}
	msg := testMessage(t, 4, "Jooa", "Kouluterveydenhoitajan info", "...", "2026-08-01")

	events, _, err := ExtractMessage(context.Background(), client, msg)
	if err != nil {
		t.Fatalf("ExtractMessage: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0: %+v", len(events), events)
	}
}

// TestExtractMessage_AudienceGuardians replays the audience_guardians.json
// fixture, which is SYNTHETIC (see testdata/README.md) rather than a live
// capture — it proves the Go-side wiring (Candidate.Audience decodes,
// BuildEvent preserves it without flagging), not that Gemini actually
// classifies real guardians-only messages this way. Do not treat this test
// passing as validation of the audience feature in production.
func TestExtractMessage_AudienceGuardians(t *testing.T) {
	url := fixtureServer(t, "audience_guardians.json")
	client, err := gemini.NewClient("test-key", "gemini-3.5-flash-lite", gemini.WithBaseURL(url))
	if err != nil {
		t.Fatal(err)
	}
	msg := testMessage(t, 6, "Ella", "Vanhempainilta 26.8.", "...", "2026-08-01")

	events, _, err := ExtractMessage(context.Background(), client, msg)
	if err != nil {
		t.Fatalf("ExtractMessage: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Audience != AudienceGuardians {
		t.Errorf("Audience = %q, want %q", ev.Audience, AudienceGuardians)
	}
	if ev.NeedsReview {
		t.Errorf("unexpected needs_review, reasons=%v", ev.ReviewReasons)
	}
	if ev.ResolvedDate != "2026-08-26" {
		t.Errorf("resolved_date = %s, want 2026-08-26", ev.ResolvedDate)
	}
}

func TestExtractMessage_MalformedModelOutputErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"not json"}]}]}`))
	}))
	defer srv.Close()
	client, err := gemini.NewClient("k", "m", gemini.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, exchange, err := ExtractMessage(context.Background(), client, testMessage(t, 5, "Ella", "x", "y", "2026-01-01"))
	if err == nil {
		t.Fatal("expected error for malformed model output")
	}
	if exchange.StatusCode != 200 {
		t.Errorf("exchange should still be populated on decode failure, got %+v", exchange)
	}
}
