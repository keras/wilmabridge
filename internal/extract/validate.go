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

// BuildEvent turns one model Candidate into a validated Event: date
// resolution, the weekday cross-check, and the cheap plausibility flags.
// It never drops a candidate for being suspicious — everything the model
// returned is preserved, with NeedsReview/ReviewReasons set so a human (or
// a future review queue) can see why.
func BuildEvent(src Source, c Candidate) Event {
	ev := Event{
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
		Location:        c.Location,
		Items:           c.Items,
		Link:            c.Link,
		Recurrence:      c.Recurrence,
		ModelConfidence: c.Confidence,
		Quote:           c.Quote,
		ExtractVer:      ExtractVer,
	}

	flag := func(reason string) {
		ev.NeedsReview = true
		ev.ReviewReasons = append(ev.ReviewReasons, reason)
	}

	day, month, ok := parseDateRaw(c.Date)
	if !ok {
		flag(fmt.Sprintf("could not parse date %q", c.Date))
	} else {
		resolved, valid := resolveDate(src.SentAt, day, month)
		if !valid {
			flag(fmt.Sprintf("%q is not a real calendar date", c.Date))
		} else {
			ev.ResolvedDate = resolved.Format("2006-01-02")

			if c.WeekdayClaim != "" {
				claimed, claimOK := normalizeWeekdayClaim(c.WeekdayClaim)
				if !claimOK {
					flag(fmt.Sprintf("unrecognized weekday_claim %q", c.WeekdayClaim))
				} else {
					match := claimed == resolved.Weekday()
					ev.WeekdayOK = &match
					if !match {
						flag(fmt.Sprintf(
							"message says %s %s, but %s is a %s — likely a typo in the source message; verify which is correct",
							c.WeekdayClaim, c.Date, ev.ResolvedDate, finnishWeekdayName[resolved.Weekday()],
						))
					}
				}
			}

			if wd := resolved.Weekday(); wd == time.Saturday || wd == time.Sunday {
				flag(fmt.Sprintf("%s falls on a weekend (%s)", ev.ResolvedDate, finnishWeekdayName[wd]))
			}

			if !src.SentAt.IsZero() && resolved.Sub(src.SentAt) > farFutureThreshold {
				flag("resolved date is more than ~9 months after the message was sent — check this isn't a retrospective mention")
			}
		}
	}

	if c.Recurrence != nil && (c.Recurrence.Freq == "" || c.Recurrence.Count == 0) {
		flag("recurrence present but missing freq/count")
	}

	return ev
}
