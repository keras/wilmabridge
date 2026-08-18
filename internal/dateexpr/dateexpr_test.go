package dateexpr

import (
	"testing"
	"time"
)

// fixedNow is a Wednesday, so "next week" has non-trivial before/after
// boundaries to check.
var fixedNow = time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC) // Wed 2026-07-15

func TestRange_Table(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		wantFrom string
		wantTo   string
	}{
		{"exact ISO date", "2026-07-19", "2026-07-19", "2026-07-19"},
		{"today", "today", "2026-07-15", "2026-07-15"},
		{"tomorrow", "tomorrow", "2026-07-16", "2026-07-16"},
		{"this week: Monday-Sunday containing today", "this week", "2026-07-13", "2026-07-19"},
		{"next week: Monday-Sunday following the current week", "next week", "2026-07-20", "2026-07-26"},
		{"case-insensitive and trims whitespace", "  ToMorrow  ", "2026-07-16", "2026-07-16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, err := Range(tc.expr, fixedNow)
			if err != nil {
				t.Fatalf("Range(%q): %v", tc.expr, err)
			}
			if from != tc.wantFrom || to != tc.wantTo {
				t.Errorf("Range(%q) = (%q, %q), want (%q, %q)", tc.expr, from, to, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestRange_NextWeekFromSunday(t *testing.T) {
	// Sunday is the last day of "this" Mon-Sun week; "next week" must still
	// land on the following Monday, not the Monday after that.
	sunday := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	from, to, err := Range("next week", sunday)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if from != "2026-07-20" || to != "2026-07-26" {
		t.Errorf("Range(\"next week\", Sunday) = (%q, %q), want (2026-07-20, 2026-07-26)", from, to)
	}
}

func TestRange_ThisWeekFromSunday(t *testing.T) {
	// Sunday is still the last day of "this" Mon-Sun week, not the start of
	// the next one.
	sunday := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	from, to, err := Range("this week", sunday)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if from != "2026-07-13" || to != "2026-07-19" {
		t.Errorf("Range(\"this week\", Sunday) = (%q, %q), want (2026-07-13, 2026-07-19)", from, to)
	}
}

func TestRange_NextWeekFromMonday(t *testing.T) {
	monday := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	from, to, err := Range("next week", monday)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if from != "2026-07-20" || to != "2026-07-26" {
		t.Errorf("Range(\"next week\", Monday) = (%q, %q), want (2026-07-20, 2026-07-26)", from, to)
	}
}

func TestRange_UnrecognizedExpressionIsAnError(t *testing.T) {
	cases := []string{"", "yesterday", "friday", "07/19/2026", "next month"}
	for _, expr := range cases {
		if _, _, err := Range(expr, fixedNow); err == nil {
			t.Errorf("Range(%q) succeeded, want an error (vocabulary is deliberately minimal)", expr)
		}
	}
}

func TestRange_LocationIsRespected(t *testing.T) {
	// A time.Location shift that crosses midnight must move "today" with
	// it -- callers pass wilma.Helsinki so this matters near local midnight.
	utcMinus5, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("America/New_York tzdata not available")
	}
	// 2026-07-15 02:00 UTC is 2026-07-14 22:00 in New York.
	lateUTC := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC).In(utcMinus5)
	from, to, err := Range("today", lateUTC)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if from != "2026-07-14" || to != "2026-07-14" {
		t.Errorf("Range(\"today\") = (%q, %q), want 2026-07-14 (New York's local date)", from, to)
	}
}
