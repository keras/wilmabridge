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

// buildOneEvent calls BuildEvent and asserts it produced exactly one Event
// — the common case in these tests. Recurrence-expansion tests call
// BuildEvent directly since they specifically want the full slice.
func buildOneEvent(t *testing.T, src Source, c Candidate) Event {
	t.Helper()
	evs := BuildEvent(src, c)
	if len(evs) != 1 {
		t.Fatalf("BuildEvent returned %d events, want 1: %+v", len(evs), evs)
	}
	return evs[0]
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
		Audience: AudienceChild, Confidence: 1.0, Quote: "to 4.3. jotain",
	}
	ev := buildOneEvent(t, Source{SentAt: sentAt}, c)
	if ev.WeekdayOK == nil || *ev.WeekdayOK {
		t.Fatalf("weekday_ok = %v, want false", ev.WeekdayOK)
	}
	if !ev.NeedsReview {
		t.Error("expected needs_review=true on weekday mismatch")
	}
	// Audience is explicitly set above, so the only reason should be the
	// weekday mismatch -- not also the audience-missing default.
	if len(ev.ReviewReasons) != 1 {
		t.Errorf("review_reasons = %v", ev.ReviewReasons)
	}
}

func TestBuildEvent_WeekdayMatchNotFlagged(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-03-01")
	c := Candidate{
		Kind: "exam", Title: "x", Date: "5.3.", WeekdayClaim: "to", // correct: Thursday
		Audience: AudienceChild, Confidence: 1.0, Quote: "to 5.3. koe",
	}
	ev := buildOneEvent(t, Source{SentAt: sentAt}, c)
	if ev.WeekdayOK == nil || !*ev.WeekdayOK {
		t.Fatalf("weekday_ok = %v, want true", ev.WeekdayOK)
	}
	if ev.NeedsReview {
		t.Errorf("unexpected needs_review, reasons=%v", ev.ReviewReasons)
	}
}

func TestBuildEvent_NoWeekdayClaimSkipsCheck(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-04-01")
	c := Candidate{Kind: "deadline", Title: "x", Date: "14.4.", Audience: AudienceChild, Confidence: 1.0, Quote: "asti 14.4."}
	ev := buildOneEvent(t, Source{SentAt: sentAt}, c)
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
	ev := buildOneEvent(t, Source{SentAt: sentAt}, c)
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
	ev := buildOneEvent(t, Source{SentAt: sentAt}, c)
	if !ev.NeedsReview {
		t.Error("expected weekend event to be flagged for review")
	}
}

func TestBuildEvent_RecurrenceMissingCountFlagged(t *testing.T) {
	// Defensive: even though schema.go requires freq+count, a malformed or
	// future response shouldn't silently pass through with a broken series.
	// A recurrence Go can't safely expand degrades to a single flagged
	// event rather than guessing a count.
	sentAt := mustParseHelsinki(t, "2026-10-01")
	c := Candidate{
		Kind: "event", Title: "uinti", Date: "10.10.", Audience: AudienceChild, Confidence: 1.0, Quote: "q",
		Recurrence: &Recurrence{Freq: "weekly"}, // Count left at zero value
	}
	ev := buildOneEvent(t, Source{SentAt: sentAt}, c)
	if !ev.NeedsReview {
		t.Error("expected needs_review=true for recurrence missing count")
	}
}

// TestBuildEvent_RecurrenceExpandsIntoIndependentEvents mirrors the real
// "Neljä uintivuoroa" case (see extract_test.go's TestExtractMessage_
// Recurrence for the full pipeline version): one recurring candidate
// becomes one Event per occurrence, each independently dated and
// weekday-checked, with no series/grouping field linking them.
func TestBuildEvent_RecurrenceExpandsIntoIndependentEvents(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-01-01")
	c := Candidate{
		Kind: "event", Title: "Uinti", Date: "5.1.", WeekdayClaim: "ma", // 5.1.2026 is genuinely a Monday
		Audience: AudienceChild, Confidence: 1.0, Quote: "q",
		Recurrence: &Recurrence{Freq: "weekly", Count: 3},
	}
	events := BuildEvent(Source{SentAt: sentAt}, c)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	wantDates := []string{"2026-01-05", "2026-01-12", "2026-01-19"}
	for i, want := range wantDates {
		ev := events[i]
		if ev.ResolvedDate != want {
			t.Errorf("event[%d].ResolvedDate = %q, want %q", i, ev.ResolvedDate, want)
		}
		if ev.DateRaw != "5.1." {
			t.Errorf("event[%d].DateRaw = %q, want the unchanged raw phrase %q on every occurrence", i, ev.DateRaw, "5.1.")
		}
		if ev.Title != "Uinti" {
			t.Errorf("event[%d].Title = %q", i, ev.Title)
		}
		// Weekly preserves the weekday, so the claim holds -- and is
		// independently checked -- for every occurrence.
		if ev.WeekdayOK == nil || !*ev.WeekdayOK {
			t.Errorf("event[%d].WeekdayOK = %v, want true", i, ev.WeekdayOK)
		}
		if ev.NeedsReview {
			t.Errorf("event[%d]: unexpected needs_review, reasons=%v", i, ev.ReviewReasons)
		}
	}
}

// TestBuildEvent_RecurrenceOccurrencesFlaggedIndependently checks that a
// freq which does NOT preserve weekday (monthly) can make one occurrence
// weekday-consistent and another not, each flagged on its own merits
// rather than the whole series inheriting one verdict.
func TestBuildEvent_RecurrenceOccurrencesFlaggedIndependently(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-01-01")
	c := Candidate{
		// 5.1.2026 is a Monday; monthly stepping lands on 5.2. (Thursday)
		// and 5.3. (also Thursday) -- neither matches the "ma" claim, so
		// occurrence 0 should be flagged for the mismatch while 1 and 2
		// should not (their own weekday_ok is simply nil-worthy... no,
		// they're still checked against the same claim and will ALSO
		// mismatch, since only 5.1. is genuinely a Monday). This exercises
		// that each occurrence computes its own weekday independently
		// rather than reusing occurrence 0's verdict.
		Kind: "event", Title: "x", Date: "5.1.", WeekdayClaim: "ma",
		Audience: AudienceChild, Confidence: 1.0, Quote: "q",
		Recurrence: &Recurrence{Freq: "monthly", Count: 2},
	}
	events := BuildEvent(Source{SentAt: sentAt}, c)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].WeekdayOK == nil || !*events[0].WeekdayOK {
		t.Errorf("event[0].WeekdayOK = %v, want true (5.1.2026 is a Monday)", events[0].WeekdayOK)
	}
	if events[1].WeekdayOK == nil || *events[1].WeekdayOK {
		t.Errorf("event[1].WeekdayOK = %v, want false (5.2.2026 is not a Monday)", events[1].WeekdayOK)
	}
	if events[0].NeedsReview {
		t.Errorf("event[0]: unexpected needs_review, reasons=%v", events[0].ReviewReasons)
	}
	if !events[1].NeedsReview {
		t.Error("event[1]: expected needs_review=true for its own weekday mismatch")
	}
}

func TestBuildEvent_RecurrenceCountIsCapped(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-01-01")
	c := Candidate{
		Kind: "event", Title: "x", Date: "5.1.", Audience: AudienceChild, Confidence: 1.0, Quote: "q",
		Recurrence: &Recurrence{Freq: "daily", Count: 9999},
	}
	events := BuildEvent(Source{SentAt: sentAt}, c)
	if len(events) != maxRecurrenceOccurrences {
		t.Fatalf("got %d events, want the cap of %d", len(events), maxRecurrenceOccurrences)
	}
	for i, ev := range events {
		if !ev.NeedsReview {
			t.Errorf("event[%d]: expected needs_review=true (count exceeds cap)", i)
		}
	}
}

func TestStepDate(t *testing.T) {
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		freq string
		n    int
		want string
	}{
		{"daily n=0 returns base unchanged", "daily", 0, "2026-01-05"},
		{"daily", "daily", 3, "2026-01-08"},
		{"weekly", "weekly", 1, "2026-01-12"},
		{"weekly x3", "weekly", 3, "2026-01-26"},
		{"biweekly", "biweekly", 1, "2026-01-19"},
		{"biweekly x2 crosses a month boundary", "biweekly", 2, "2026-02-02"},
		{"monthly", "monthly", 1, "2026-02-05"},
		{"monthly x3", "monthly", 3, "2026-04-05"},
		{"unrecognized freq falls back to weekly", "fortnightly", 1, "2026-01-12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stepDate(base, tc.freq, tc.n)
			if got.Format("2006-01-02") != tc.want {
				t.Errorf("stepDate(base, %q, %d) = %s, want %s", tc.freq, tc.n, got.Format("2006-01-02"), tc.want)
			}
		})
	}
}

// TestStepDate_MonthlyDayOverflowRolls documents (doesn't "fix") a genuine
// edge case: Go's time.AddDate normalizes an out-of-range day into the
// following month, so a monthly series anchored on the 31st can silently
// land on a different day of month once it crosses a shorter month. Worth
// knowing about; not something this package tries to paper over, since a
// real monthly-on-the-31st recurrence hasn't turned up in real data.
func TestStepDate_MonthlyDayOverflowRolls(t *testing.T) {
	base := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	got := stepDate(base, "monthly", 1)
	if want := "2026-03-03"; got.Format("2006-01-02") != want {
		t.Errorf("stepDate(31 Jan, monthly, 1) = %s, want %s (2026's February has 28 days, so day 31 overflows into March)",
			got.Format("2006-01-02"), want)
	}
}

func TestBuildEvent_AudienceDefaultsToChildAndFlags(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-04-01")
	cases := []struct {
		name        string
		audience    string
		wantFlagged bool
	}{
		{"empty", "", true},
		{"unrecognized", "parents", true},
		{"exact child", AudienceChild, false},
		{"exact guardians", AudienceGuardians, false},
		{"tolerant case/whitespace child", "CHILD ", false},
		{"tolerant case/whitespace guardians", " Guardians", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Candidate{Kind: "event", Title: "x", Date: "14.4.", Audience: tc.audience, Confidence: 1.0, Quote: "q"}
			ev := buildOneEvent(t, Source{SentAt: sentAt}, c)
			if ev.NeedsReview != tc.wantFlagged {
				t.Errorf("NeedsReview = %v, want %v (reasons=%v)", ev.NeedsReview, tc.wantFlagged, ev.ReviewReasons)
			}
			// Audience is never left empty on the output Event, whatever the
			// model sent -- the pipeline downstream (event_children fan-out)
			// has no representation for "unknown".
			if ev.Audience != AudienceChild && ev.Audience != AudienceGuardians {
				t.Errorf("Audience = %q, want a known category", ev.Audience)
			}
		})
	}
}

func TestBuildEvent_AudienceGuardiansPreserved(t *testing.T) {
	sentAt := mustParseHelsinki(t, "2026-04-01")
	c := Candidate{Kind: "event", Title: "Vanhempainilta", Date: "14.4.", Audience: AudienceGuardians, Confidence: 1.0, Quote: "q"}
	ev := buildOneEvent(t, Source{SentAt: sentAt}, c)
	if ev.Audience != AudienceGuardians {
		t.Errorf("Audience = %q, want %q", ev.Audience, AudienceGuardians)
	}
	if ev.NeedsReview {
		t.Errorf("unexpected needs_review, reasons=%v", ev.ReviewReasons)
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
