package extract

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// farFutureThreshold flags a resolved date this far past the message's
// send date for review. Real school messages refer to near-term events; a
// date resolving further out than this is more likely a retrospective
// mention ("koe siirtyi, oli 4.3.") landing a year ahead than a genuine
// long-range notice — cheap enough to just flag rather than build separate
// retrospective-detection machinery for what should be a rare case.
const farFutureThreshold = 270 * 24 * time.Hour // ~9 months

var dateRawPattern = regexp.MustCompile(`^\s*(\d{1,2})\.(\d{1,2})\.?\s*$`)

// parseDateRaw parses a bare Finnish day.month date like "4.3." or "10.10.".
// It intentionally does not accept a year — the model is instructed never
// to supply one (see prompt.go), so a raw value carrying one is treated as
// unparseable rather than silently accepted in some other format.
func parseDateRaw(raw string) (day, month int, ok bool) {
	m := dateRawPattern.FindStringSubmatch(raw)
	if m == nil {
		return 0, 0, false
	}
	var d, mo int
	if _, err := fmt.Sscanf(m[1], "%d", &d); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(m[2], "%d", &mo); err != nil {
		return 0, 0, false
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return 0, 0, false
	}
	return d, mo, true
}

// resolveDate anchors (day, month) to the next occurrence on or after
// sentAt's calendar date, per the design's single rule: real school
// messages refer to near-term events, so no weekday-based disambiguation is
// needed — see openclaw-integration.md's "Year resolution — one rule".
//
// valid is false if (day, month) is not a real calendar date in either
// candidate year (e.g. day=31, month=4): Go's time.Date silently normalizes
// out-of-range days into the following month, which would otherwise turn a
// misread "31.4." into a confidently-wrong "1.5." with no signal that
// anything was off.
func resolveDate(sentAt time.Time, day, month int) (resolved time.Time, valid bool) {
	loc := sentAt.Location()
	if loc == nil {
		loc = time.UTC
	}
	sentDate := time.Date(sentAt.Year(), sentAt.Month(), sentAt.Day(), 0, 0, 0, 0, loc)

	try := func(year int) (time.Time, bool) {
		t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
		return t, int(t.Month()) == month && t.Day() == day
	}

	candidate, ok := try(sentAt.Year())
	if !ok {
		return time.Time{}, false
	}
	if candidate.Before(sentDate) {
		candidate, ok = try(sentAt.Year() + 1)
		if !ok {
			return time.Time{}, false
		}
	}
	return candidate, true
}

// finnishWeekdayAbbrev / finnishWeekdayName map Go's time.Weekday to the
// Finnish abbreviations the model is asked for (ma/ti/ke/to/pe/la/su) and
// full names, for building human-readable review reasons.
var (
	finnishWeekdayAbbrev = map[time.Weekday]string{
		time.Monday: "ma", time.Tuesday: "ti", time.Wednesday: "ke",
		time.Thursday: "to", time.Friday: "pe", time.Saturday: "la", time.Sunday: "su",
	}
	finnishWeekdayName = map[time.Weekday]string{
		time.Monday: "maanantai", time.Tuesday: "tiistai", time.Wednesday: "keskiviikko",
		time.Thursday: "torstai", time.Friday: "perjantai", time.Saturday: "lauantai", time.Sunday: "sunnuntai",
	}
	abbrevToWeekday = func() map[string]time.Weekday {
		m := make(map[string]time.Weekday, 7)
		for wd, abbr := range finnishWeekdayAbbrev {
			m[abbr] = wd
		}
		return m
	}()
)

// normalizeWeekdayClaim parses a weekday_claim value (case-insensitive,
// trimmed) into a time.Weekday.
func normalizeWeekdayClaim(claim string) (time.Weekday, bool) {
	wd, ok := abbrevToWeekday[strings.ToLower(strings.TrimSpace(claim))]
	return wd, ok
}

// maxRecurrenceOccurrences caps how many Events one recurring Candidate can
// expand into. Every real message seen so far tops out at 4 (see
// testdata/README.md); this exists purely as a backstop against a
// hallucinated or malformed count (e.g. the model returning 9999) flooding
// the database with rows, not as a judgment about what's realistic —
// generous on purpose.
const maxRecurrenceOccurrences = 52 // a year of weekly occurrences

// Source is the message-level context BuildEvent needs to resolve and
// annotate a Candidate.
type Source struct {
	WilmaID        int64
	Child          string
	Role           string
	MessageSubject string
	MessageURL     string
	SentAt         time.Time
}

// stepDate returns the date of the n-th occurrence (n >= 0, n == 0 is base
// itself) of a series starting at base, per freq — one of the values
// schema.go's recurrence.freq enum allows. An unrecognized freq (shouldn't
// happen; the schema enum should prevent the model from sending one) falls
// back to weekly rather than returning base unchanged for every n, which
// would silently collapse a whole series onto one date.
func stepDate(base time.Time, freq string, n int) time.Time {
	switch freq {
	case "daily":
		return base.AddDate(0, 0, n)
	case "biweekly":
		return base.AddDate(0, 0, 14*n)
	case "monthly":
		return base.AddDate(0, n, 0)
	default: // "weekly", or anything unrecognized
		return base.AddDate(0, 0, 7*n)
	}
}

// applyDateChecks runs the weekday cross-check and the cheap plausibility
// flags (weekend, far-future) against one occurrence's resolved date. Used
// once for a non-recurring candidate and once per occurrence for a
// recurring one — each occurrence is checked independently against its own
// actual weekday, since e.g. a monthly series can land on a different
// weekday each time even though weekly/biweekly always preserve it.
func applyDateChecks(ev *Event, sentAt time.Time, weekdayClaim string, resolved time.Time) {
	flag := func(reason string) {
		ev.NeedsReview = true
		ev.ReviewReasons = append(ev.ReviewReasons, reason)
	}

	if weekdayClaim != "" {
		claimed, claimOK := normalizeWeekdayClaim(weekdayClaim)
		if !claimOK {
			flag(fmt.Sprintf("unrecognized weekday_claim %q", weekdayClaim))
		} else {
			match := claimed == resolved.Weekday()
			ev.WeekdayOK = &match
			if !match {
				flag(fmt.Sprintf(
					"message says %s %s, but %s is a %s — likely a typo in the source message; verify which is correct",
					weekdayClaim, ev.DateRaw, ev.ResolvedDate, finnishWeekdayName[resolved.Weekday()],
				))
			}
		}
	}

	if wd := resolved.Weekday(); wd == time.Saturday || wd == time.Sunday {
		flag(fmt.Sprintf("%s falls on a weekend (%s)", ev.ResolvedDate, finnishWeekdayName[wd]))
	}

	if !sentAt.IsZero() && resolved.Sub(sentAt) > farFutureThreshold {
		flag("resolved date is more than ~9 months after the message was sent — check this isn't a retrospective mention")
	}
}

// BuildEvent turns one model Candidate into one or more validated Events.
// Ordinarily that's one Event. A recurring candidate (Recurrence present
// and valid) becomes one Event PER OCCURRENCE — dated by stepping forward
// from the first occurrence per Recurrence.Freq — each independently date-
// resolved, weekday-checked, and flagged; there is no series/grouping
// field linking them back together, by design. It never drops a candidate
// for being suspicious — everything the model returned is preserved, with
// NeedsReview/ReviewReasons set on the relevant event(s) so a human (or a
// future review queue) can see why.
func BuildEvent(src Source, c Candidate) []Event {
	base := Event{
		WilmaID:         src.WilmaID,
		Child:           src.Child,
		Role:            src.Role,
		MessageSubject:  src.MessageSubject,
		MessageURL:      src.MessageURL,
		Kind:            c.Kind,
		Title:           c.Title,
		Detail:          c.Detail,
		DateRaw:         c.Date,
		WeekdayClaim:    c.WeekdayClaim,
		Time:            c.Time,
		Link:            c.Link,
		ModelConfidence: c.Confidence,
		Quote:           c.Quote,
		ExtractVer:      ExtractVer,
	}

	switch strings.ToLower(strings.TrimSpace(c.Audience)) {
	case AudienceChild:
		base.Audience = AudienceChild
	case AudienceGuardians:
		base.Audience = AudienceGuardians
	default:
		// Same never-silently-guess philosophy as the date/weekday
		// validators below. "child" is the conservative default — it's the
		// pre-audience behavior (fan out to everyone who received the
		// message), and over-notifying beats under-notifying — but the
		// guess is made visible rather than hidden. Applied to base so
		// every occurrence of a recurring candidate carries it.
		base.Audience = AudienceChild
		base.NeedsReview = true
		base.ReviewReasons = append(base.ReviewReasons, fmt.Sprintf("audience missing or unrecognized (%q), defaulting to child", c.Audience))
	}

	day, month, ok := parseDateRaw(c.Date)
	if !ok {
		ev := base
		ev.NeedsReview = true
		ev.ReviewReasons = append(ev.ReviewReasons, fmt.Sprintf("could not parse date %q", c.Date))
		return []Event{ev}
	}

	firstOccurrence, valid := resolveDate(src.SentAt, day, month)
	if !valid {
		ev := base
		ev.NeedsReview = true
		ev.ReviewReasons = append(ev.ReviewReasons, fmt.Sprintf("%q is not a real calendar date", c.Date))
		return []Event{ev}
	}

	if c.Recurrence == nil {
		ev := base
		ev.ResolvedDate = firstOccurrence.Format("2006-01-02")
		applyDateChecks(&ev, src.SentAt, c.WeekdayClaim, firstOccurrence)
		return []Event{ev}
	}

	if c.Recurrence.Freq == "" || c.Recurrence.Count == 0 {
		ev := base
		ev.ResolvedDate = firstOccurrence.Format("2006-01-02")
		applyDateChecks(&ev, src.SentAt, c.WeekdayClaim, firstOccurrence)
		ev.NeedsReview = true
		ev.ReviewReasons = append(ev.ReviewReasons, "recurrence present but missing freq/count")
		return []Event{ev}
	}

	count := c.Recurrence.Count
	truncated := count > maxRecurrenceOccurrences
	if truncated {
		count = maxRecurrenceOccurrences
	}

	events := make([]Event, 0, count)
	for i := 0; i < count; i++ {
		occurrence := firstOccurrence
		if i > 0 {
			occurrence = stepDate(firstOccurrence, c.Recurrence.Freq, i)
		}
		ev := base
		ev.ResolvedDate = occurrence.Format("2006-01-02")
		applyDateChecks(&ev, src.SentAt, c.WeekdayClaim, occurrence)
		if truncated {
			ev.NeedsReview = true
			ev.ReviewReasons = append(ev.ReviewReasons, fmt.Sprintf(
				"recurrence count %d exceeds the %d-occurrence cap; only %d emitted", c.Recurrence.Count, maxRecurrenceOccurrences, maxRecurrenceOccurrences,
			))
		}
		events = append(events, ev)
	}
	return events
}
