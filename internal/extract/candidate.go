package extract

// Recurrence mirrors the model's {freq, count} shape. Note: this iteration
// deliberately does NOT expand a recurrence into multiple dated occurrences
// — the user flagged the multi-week swimming example as a synthetic stress
// test, not representative of real messages, so building and testing
// expansion logic now would be effort spent on a case that hasn't shown up
// in real data. The raw hint is preserved on the output Event so a human
// (or a future expansion step) has it, but only ONE Event is emitted per
// candidate regardless of Count.
type Recurrence struct {
	Freq  string `json:"freq"`
	Count int    `json:"count"`
}

// Candidate is exactly what the model returns for one event, per
// schema.go's responseSchema.
type Candidate struct {
	Kind         string      `json:"kind"`
	Title        string      `json:"title"`
	Detail       string      `json:"detail,omitempty"`
	Date         string      `json:"date"`
	WeekdayClaim string      `json:"weekday_claim,omitempty"`
	Time         string      `json:"time,omitempty"`
	Location     string      `json:"location,omitempty"`
	Items        []string    `json:"items,omitempty"`
	Link         string      `json:"link,omitempty"`
	Recurrence   *Recurrence `json:"recurrence,omitempty"`
	Confidence   float64     `json:"confidence"`
	Quote        string      `json:"quote"`
}

type candidatesResponse struct {
	Candidates []Candidate `json:"candidates"`
}

// Event is one wilmabridge-extract output record: a Candidate plus
// everything Go computed from it (date resolution, weekday cross-check,
// review flags) and enough source-message context to stand alone once
// flattened to NDJSON.
type Event struct {
	// Source message context.
	WilmaID        int64  `json:"wilma_id"`
	Child          string `json:"child"`
	Role           string `json:"role"`
	MessageSubject string `json:"message_subject"`
	MessageURL     string `json:"message_url"`

	// From the model, unchanged.
	Kind            string      `json:"kind"`
	Title           string      `json:"title"`
	Detail          string      `json:"detail,omitempty"`
	DateRaw         string      `json:"date_raw"`
	WeekdayClaim    string      `json:"weekday_claim,omitempty"`
	Time            string      `json:"time,omitempty"`
	Location        string      `json:"location,omitempty"`
	Items           []string    `json:"items,omitempty"`
	Link            string      `json:"link,omitempty"`
	Recurrence      *Recurrence `json:"recurrence,omitempty"`
	ModelConfidence float64     `json:"model_confidence,omitempty"`
	Quote           string      `json:"quote"`

	// Computed by Go (see validate.go). ResolvedDate is "" and WeekdayOK is
	// nil whenever DateRaw couldn't be parsed — never guessed.
	ResolvedDate  string   `json:"resolved_date,omitempty"`
	WeekdayOK     *bool    `json:"weekday_ok,omitempty"`
	NeedsReview   bool     `json:"needs_review"`
	ReviewReasons []string `json:"review_reasons,omitempty"`

	ExtractVer int `json:"extract_ver"`
}
