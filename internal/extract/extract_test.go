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
		if ev.NeedsReview {
			t.Errorf("event[%d] unexpectedly needs review: %v", i, ev.ReviewReasons)
		}
		if ev.Child != "Ella" || ev.WilmaID != 1 {
			t.Errorf("event[%d] source context = child=%q wilma_id=%d", i, ev.Child, ev.WilmaID)
		}
	}
	// Items were only stated on the sports-day and library-trip lines, not
	// the exam or St. Patrick's — matches the "items only if explicit" rule.
	if len(events[1].Items) != 0 {
		t.Errorf("exam should have no items, got %v", events[1].Items)
	}
	if len(events[3].Items) != 2 {
		t.Errorf("library trip should have 2 items, got %v", events[3].Items)
	}
}

// TestExtractMessage_Recurrence replays the real (post-fix) capture proving
// the schema's required:[freq,count] actually recovers the count — see
// schema.go's doc comment for the failure this guards against.
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
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (no expansion in this iteration): %+v", len(events), events)
	}
	ev := events[0]
	if ev.Recurrence == nil {
		t.Fatal("expected recurrence to be preserved on the event")
	}
	if ev.Recurrence.Freq != "weekly" || ev.Recurrence.Count != 4 {
		t.Errorf("recurrence = %+v, want {weekly 4}", ev.Recurrence)
	}
	if ev.ResolvedDate != "2026-10-10" {
		t.Errorf("resolved_date = %s, want 2026-10-10", ev.ResolvedDate)
	}
	// 10.10.2026 is genuinely a Saturday: the weekend validator correctly
	// flags this rather than silently scheduling swimming during a weekend
	// PE lesson. NeedsReview=true here is the validator working, not a bug.
	if !ev.NeedsReview {
		t.Error("expected needs_review=true: 10.10.2026 is a Saturday")
	}
	if len(ev.Items) != 3 {
		t.Errorf("items = %v, want 3", ev.Items)
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
