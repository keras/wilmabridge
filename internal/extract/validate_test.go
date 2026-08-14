package extract

import (
	"testing"
	"time"

	"wilmabridge/internal/wilma"
)

func mustParseHelsinki(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02", s, wilma.Helsinki)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return tm
}

func TestParseDateRaw(t *testing.T) {
	cases := []struct {
		raw                string
		wantDay, wantMonth int
		wantOK             bool
	}{
		{"4.3.", 4, 3, true},
		{"4.3", 4, 3, true}, // trailing dot optional
		{"10.10.", 10, 10, true},
		{"31.12.", 31, 12, true},
		{"garbage", 0, 0, false},
		{"", 0, 0, false},
		{"4.3.2026", 0, 0, false}, // model must never send a year; reject if it does
		{"32.1.", 0, 0, false},    // day out of range
		{"1.13.", 0, 0, false},    // month out of range
	}
	for _, tc := range cases {
		day, month, ok := parseDateRaw(tc.raw)
		if ok != tc.wantOK || day != tc.wantDay || month != tc.wantMonth {
			t.Errorf("parseDateRaw(%q) = (%d,%d,%v), want (%d,%d,%v)",
				tc.raw, day, month, ok, tc.wantDay, tc.wantMonth, tc.wantOK)
		}
	}
}

// TestResolveDate_MonthlyLetterDates anchors on the exact dates verified by
// hand for the monthly-letter worked example: a message sent in early 2026
// referencing ke 4.3., to 5.3., ti 17.3., pe 20.3. — all of which are
// genuinely those weekdays in 2026 (confirmed with `date -d`).
func TestResolveDate_MonthlyLetterDates(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-03-01")
	cases := []struct {
		day, month  int
		wantDate    string
		wantWeekday time.Weekday
	}{
		{4, 3, "2026-03-04", time.Wednesday},
		{5, 3, "2026-03-05", time.Thursday},
		{17, 3, "2026-03-17", time.Tuesday},
		{20, 3, "2026-03-20", time.Friday},
	}
	for _, tc := range cases {
		got, valid := resolveDate(sentAt, tc.day, tc.month)
		if !valid {
			t.Fatalf("resolveDate(%d.%d.) not valid", tc.day, tc.month)
		}
		if got.Format("2006-01-02") != tc.wantDate {
			t.Errorf("resolveDate(%d.%d.) = %s, want %s", tc.day, tc.month, got.Format("2006-01-02"), tc.wantDate)
		}
		if got.Weekday() != tc.wantWeekday {
			t.Errorf("%s weekday = %s, want %s", tc.wantDate, got.Weekday(), tc.wantWeekday)
		}
	}
}

func TestResolveDate_RollsToNextYear(t *testing.T) {
	// A message sent in December referring to a January date should resolve
	// forward into next year, not backward into the past.
	sentAt := mustParseHelsinki(t, "2026-12-20")
	got, valid := resolveDate(sentAt, 7, 1)
	if !valid {
		t.Fatal("not valid")
	}
	if want := "2027-01-07"; got.Format("2006-01-02") != want {
		t.Errorf("resolveDate = %s, want %s", got.Format("2006-01-02"), want)
	}
}

func TestResolveDate_SameDayCountsAsOnOrAfter(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-03-04")
	got, valid := resolveDate(sentAt, 4, 3)
	if !valid || got.Format("2006-01-02") != "2026-03-04" {
		t.Errorf("resolveDate same-day = %v valid=%v, want 2026-03-04", got, valid)
	}
}

func TestResolveDate_InvalidCalendarDate(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-04-01")
	// April has 30 days; without the validity check, Go's time.Date would
	// silently roll 31.4. into 1.5.
	_, valid := resolveDate(sentAt, 31, 4)
	if valid {
		t.Error("31.4. should not resolve to a valid date")
	}
}

func TestBuildEvent_WeekdayMismatchFlagged(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-03-01")
	c := Candidate{
		Kind: "event", Title: "x", Date: "4.3.", WeekdayClaim: "to", // 4.3.2026 is actually Wednesday
		Confidence: 1.0, Quote: "to 4.3. jotain",
	}
	ev := BuildEvent(Source{SentAt: sentAt}, c)
	if ev.WeekdayOK == nil || *ev.WeekdayOK {
		t.Fatalf("weekday_ok = %v, want false", ev.WeekdayOK)
	}
	if !ev.NeedsReview {
		t.Error("expected needs_review=true on weekday mismatch")
	}
	if len(ev.ReviewReasons) != 1 {
		t.Errorf("review_reasons = %v", ev.ReviewReasons)
	}
}

func TestBuildEvent_WeekdayMatchNotFlagged(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-03-01")
	c := Candidate{
		Kind: "exam", Title: "x", Date: "5.3.", WeekdayClaim: "to", // correct: Thursday
		Confidence: 1.0, Quote: "to 5.3. koe",
	}
	ev := BuildEvent(Source{SentAt: sentAt}, c)
	if ev.WeekdayOK == nil || !*ev.WeekdayOK {
		t.Fatalf("weekday_ok = %v, want true", ev.WeekdayOK)
	}
	if ev.NeedsReview {
		t.Errorf("unexpected needs_review, reasons=%v", ev.ReviewReasons)
	}
}

func TestBuildEvent_NoWeekdayClaimSkipsCheck(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-04-01")
	c := Candidate{Kind: "deadline", Title: "x", Date: "14.4.", Confidence: 1.0, Quote: "asti 14.4."}
	ev := BuildEvent(Source{SentAt: sentAt}, c)
	if ev.WeekdayOK != nil {
		t.Errorf("weekday_ok = %v, want nil (no claim to check)", ev.WeekdayOK)
	}
	if ev.NeedsReview {
		t.Errorf("unexpected needs_review, reasons=%v", ev.ReviewReasons)
	}
	if ev.ResolvedDate != "2026-04-14" {
		t.Errorf("resolved_date = %s", ev.ResolvedDate)
	}
}

func TestBuildEvent_UnparseableDateFlaggedNotGuessed(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-04-01")
	c := Candidate{Kind: "event", Title: "x", Date: "ensi viikolla", Confidence: 1.0, Quote: "q"}
	ev := BuildEvent(Source{SentAt: sentAt}, c)
	if ev.ResolvedDate != "" {
		t.Errorf("resolved_date = %q, want empty", ev.ResolvedDate)
	}
	if !ev.NeedsReview {
		t.Error("expected needs_review=true")
	}
}

func TestBuildEvent_WeekendFlagged(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-05-01")
	// 2026-05-09 is a Saturday.
	c := Candidate{Kind: "event", Title: "kevätrieha", Date: "9.5.", Confidence: 1.0, Quote: "q"}
	ev := BuildEvent(Source{SentAt: sentAt}, c)
	if !ev.NeedsReview {
		t.Error("expected weekend event to be flagged for review")
	}
}

func TestBuildEvent_RecurrenceMissingCountFlagged(t *testing.T) {
	// Defensive: even though schema.go requires freq+count, a malformed or
	// future response shouldn't silently pass through with a broken series.
	sentAt := mustParseHelsinki(t, "2026-10-01")
	c := Candidate{
		Kind: "event", Title: "uinti", Date: "10.10.", Confidence: 1.0, Quote: "q",
		Recurrence: &Recurrence{Freq: "weekly"}, // Count left at zero value
	}
	ev := BuildEvent(Source{SentAt: sentAt}, c)
	if !ev.NeedsReview {
		t.Error("expected needs_review=true for recurrence missing count")
	}
}

func TestNormalizeWeekdayClaim(t *testing.T) {
	cases := map[string]time.Weekday{
		"ma": time.Monday, "TI": time.Tuesday, " ke ": time.Wednesday,
		"to": time.Thursday, "pe": time.Friday, "la": time.Saturday, "su": time.Sunday,
	}
	for claim, want := range cases {
		got, ok := normalizeWeekdayClaim(claim)
		if !ok || got != want {
			t.Errorf("normalizeWeekdayClaim(%q) = (%v,%v), want (%v,true)", claim, got, ok, want)
		}
	}
	if _, ok := normalizeWeekdayClaim("keskiviikko"); ok {
		t.Error("full Finnish weekday names should not be accepted (model is asked for abbreviations only)")
	}
}
