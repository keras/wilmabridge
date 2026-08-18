// Package dateexpr resolves a small, fixed vocabulary of date expressions —
// an exact ISO date, "today", "tomorrow", "this week", "next week" — into
// an inclusive [from, to] range of ISO dates (YYYY-MM-DD), the same format
// events.resolved_date is stored in (see internal/extract/validate.go). It
// exists so cmd/wilmabridge's "events" command can turn a human-friendly
// --on value into the range store.EventFilter needs, without
// cmd/wilmabridge itself knowing anything about calendar arithmetic.
//
// The vocabulary is deliberately small: an expression that isn't recognized
// is an error, not a guess. Extend it here as real usage demands more (e.g.
// "yesterday", weekday names) rather than guessing ahead of need.
package dateexpr

import (
	"fmt"
	"strings"
	"time"
)

const isoLayout = "2006-01-02"

// Range resolves expr to an inclusive [from, to] range of ISO dates,
// anchored on now's date in now's location — callers should pass now in
// the timezone events are meant to be interpreted in (wilma.Helsinki
// throughout the rest of this codebase). Comparison is case-insensitive
// and ignores surrounding whitespace.
func Range(expr string, now time.Time) (from, to string, err error) {
	e := strings.ToLower(strings.TrimSpace(expr))
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	switch e {
	case "":
		return "", "", fmt.Errorf("dateexpr: empty expression")
	case "today":
		d := today.Format(isoLayout)
		return d, d, nil
	case "tomorrow":
		d := today.AddDate(0, 0, 1).Format(isoLayout)
		return d, d, nil
	case "this week", "next week":
		// Monday-Sunday, matching the Finnish/ISO week convention the rest
		// of this codebase assumes. Weekday() is Sunday=0..Saturday=6;
		// (weekday+6)%7 is days since the most recent Monday.
		sinceMonday := (int(today.Weekday()) + 6) % 7
		thisMonday := today.AddDate(0, 0, -sinceMonday)
		monday := thisMonday
		if e == "next week" {
			monday = thisMonday.AddDate(0, 0, 7)
		}
		sunday := monday.AddDate(0, 0, 6)
		return monday.Format(isoLayout), sunday.Format(isoLayout), nil
	}

	if d, err := time.Parse(isoLayout, e); err == nil {
		iso := d.Format(isoLayout)
		return iso, iso, nil
	}

	return "", "", fmt.Errorf(`dateexpr: unrecognized expression %q (want an ISO date like "2026-07-19", "today", "tomorrow", "this week", or "next week")`, expr)
}
