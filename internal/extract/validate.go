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

var dateRawPattern = regexp.MustCompile(`^\s*(\d{1,2})\.(\d{1,2})(?:\.(\d{4})?)?\s*$`)

// parseDateRaw parses a bare Finnish day.month date like "4.3." or "10.10.",
// optionally followed by a 4-digit year like "1.9.2026". year is 0 when the
// raw text didn't carry one — the model is told never to fabricate a year,
// but school messages sometimes do include one in the source text (e.g.
// "1.9.2026"), and that's a real, parseable date rather than something to
// reject; resolveDate treats year==0 as "anchor forward from sentAt" and a
// nonzero year as an explicit, authoritative value.
func parseDateRaw(raw string) (day, month, year int, ok bool) {
	m := dateRawPattern.FindStringSubmatch(raw)
	if m == nil {
		return 0, 0, 0, false
	}
	var d, mo int
	if _, err := fmt.Sscanf(m[1], "%d", &d); err != nil {
		return 0, 0, 0, false
	}
	if _, err := fmt.Sscanf(m[2], "%d", &mo); err != nil {
		return 0, 0, 0, false
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return 0, 0, 0, false
	}
	var y int
	if m[3] != "" {
		if _, err := fmt.Sscanf(m[3], "%d", &y); err != nil {
			return 0, 0, 0, false
		}
	}
	return d, mo, y, true
}

// finnishRelativeDayOffset maps the near-term relative-day words school
// messages actually use ("koe on huomenna") to an offset in days from the
// message's send date. Weekday names ("keskiviikkona") and vaguer phrases
// ("ensi viikolla") are deliberately not covered — the model isn't asked to
// normalize those, and guessing at them risks a confidently wrong date with
// no signal anything was off.
var finnishRelativeDayOffset = map[string]int{
	"tänään":      0,
	"huomenna":    1,
	"ylihuomenna": 2,
}

// parseRelativeDate resolves a bare relative-day word against sentAt's
// calendar date. ok is false for anything not in finnishRelativeDayOffset.
func parseRelativeDate(raw string, sentAt time.Time) (resolved time.Time, ok bool) {
	offset, found := finnishRelativeDayOffset[strings.ToLower(strings.TrimSpace(raw))]
	if !found {
		return time.Time{}, false
	}
	loc := sentAt.Location()
	if loc == nil {
		loc = time.UTC
	}
	sentDate := time.Date(sentAt.Year(), sentAt.Month(), sentAt.Day(), 0, 0, 0, 0, loc)
	return sentDate.AddDate(0, 0, offset), true
}

// resolveDate turns (day, month, year) into a concrete date. When year is 0
// (not present in the raw text), it anchors (day, month) to the next
// occurrence on or after sentAt's calendar date, per the design's single
// rule: real school messages refer to near-term events, so no weekday-based
// disambiguation is needed — see openclaw-integration.md's "Year resolution
// — one rule". When year is nonzero (the raw text carried one, e.g.
// "1.9.2026"), it's used as-is rather than anchored/guessed — the message
// already said which year it meant.
//
// valid is false if (day, month[, year]) is not a real calendar date: Go's
// time.Date silently normalizes out-of-range days into the following
// month, which would otherwise turn a misread "31.4." into a confidently-
// wrong "1.5." with no signal that anything was off.
func resolveDate(sentAt time.Time, day, month, year int) (resolved time.Time, valid bool) {
	loc := sentAt.Location()
	if loc == nil {
		loc = time.UTC
	}

	try := func(y int) (time.Time, bool) {
		t := time.Date(y, time.Month(month), day, 0, 0, 0, 0, loc)
		return t, int(t.Month()) == month && t.Day() == day
	}

	if year != 0 {
		return try(year)
	}

	sentDate := time.Date(sentAt.Year(), sentAt.Month(), sentAt.Day(), 0, 0, 0, 0, loc)

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

	var firstOccurrence time.Time
	if day, month, year, ok := parseDateRaw(c.Date); ok {
		resolved, valid := resolveDate(src.SentAt, day, month, year)
		if !valid {
			ev := base
			ev.NeedsReview = true
			ev.ReviewReasons = append(ev.ReviewReasons, fmt.Sprintf("%q is not a real calendar date", c.Date))
			return []Event{ev}
		}
		firstOccurrence = resolved
	} else if rel, relOK := parseRelativeDate(c.Date, src.SentAt); relOK {
		firstOccurrence = rel
	} else {
		ev := base
		ev.NeedsReview = true
		ev.ReviewReasons = append(ev.ReviewReasons, fmt.Sprintf("could not parse date %q", c.Date))
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
