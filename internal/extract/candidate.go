package extract

// Recurrence mirrors the model's {freq, count} shape. It exists only as
// input: BuildEvent (validate.go) uses it to expand a recurring candidate
// into one Event per occurrence, each with its own resolved date — it is
// not itself stored on the output Event. See stepDate in validate.go for
// how an occurrence's date is derived from freq.
type Recurrence struct {
	Freq  string `json:"freq"`
	Count int    `json:"count"`
}

// Audience categories a Candidate can be classified into. The model is
// never told which children received the message (see prompt.go and
// openclaw-integration.md's "the child association comes from Wilma
// metadata, never from the model") — it only classifies who an event is
// for; resolving that into an actual list of children is internal/store's
// job, via message_children.
const (
	AudienceChild     = "child"
	AudienceGuardians = "guardians"
)

// Candidate is exactly what the model returns for one event, per
// schema.go's responseSchema. Location and packing-list-style detail are
// deliberately NOT structured fields here — the model is asked to fold
// that into Detail (or it's already present in Quote's verbatim excerpt)
// rather than parse it into its own property, since in practice that
// information reads fine as prose and a dedicated field just doubled the
// places it could go missing or drift out of sync with the quote.
type Candidate struct {
	Kind         string      `json:"kind"`
	Title        string      `json:"title"`
	Detail       string      `json:"detail,omitempty"`
	Date         string      `json:"date"`
	WeekdayClaim string      `json:"weekday_claim,omitempty"`
	Time         string      `json:"time,omitempty"`
	Link         string      `json:"link,omitempty"`
	Recurrence   *Recurrence `json:"recurrence,omitempty"`
	Audience     string      `json:"audience"`
	Confidence   float64     `json:"confidence"`
	Quote        string      `json:"quote"`
}

type candidatesResponse struct {
	Candidates []Candidate `json:"candidates"`
}

// Event is one wilmabridge-extract output record: a Candidate plus
// everything Go computed from it (date resolution, weekday cross-check,
// review flags) and enough source-message context to stand alone once
// flattened to NDJSON. A single recurring Candidate (see Recurrence)
// produces one Event PER OCCURRENCE, each a fully independent row with its
// own resolved date and its own review flags — there is no series/grouping
// id, by design; see validate.go's BuildEvent.
type Event struct {
	// Source message context.
	WilmaID        int64  `json:"wilma_id"`
	Child          string `json:"child"`
	Role           string `json:"role"`
	MessageSubject string `json:"message_subject"`
	MessageURL     string `json:"message_url"`

	// From the model, unchanged (DateRaw is the same raw phrase for every
	// occurrence of a recurring candidate — it's what the message actually
	// said; ResolvedDate below is what differs per occurrence).
	Kind            string  `json:"kind"`
	Title           string  `json:"title"`
	Detail          string  `json:"detail,omitempty"`
	DateRaw         string  `json:"date_raw"`
	WeekdayClaim    string  `json:"weekday_claim,omitempty"`
	Time            string  `json:"time,omitempty"`
	Link            string  `json:"link,omitempty"`
	ModelConfidence float64 `json:"model_confidence,omitempty"`
	Quote           string  `json:"quote"`

	// Audience is "child" (concerns the student; internal/store fans this
	// out to every child who received the message) or "guardians"
	// (parents-only, e.g. a vanhempainilta notice: zero children). Never
	// empty on an Event returned by BuildEvent — an unrecognized or missing
	// value from the model defaults to AudienceChild and is flagged for
	// review rather than silently guessed.
	Audience string `json:"audience"`

	// Computed by Go (see validate.go). ResolvedDate is "" and WeekdayOK is
	// nil whenever DateRaw couldn't be parsed — never guessed.
	ResolvedDate  string   `json:"resolved_date,omitempty"`
	WeekdayOK     *bool    `json:"weekday_ok,omitempty"`
	NeedsReview   bool     `json:"needs_review"`
	ReviewReasons []string `json:"review_reasons,omitempty"`

	ExtractVer int `json:"extract_ver"`
}
