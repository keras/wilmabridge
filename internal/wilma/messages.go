package wilma

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Helsinki is the timezone Wilma's TimeStamp fields are assumed to be in.
// Falls back to a fixed +02:00/+03:00-less UTC offset only if the tzdata
// database is unavailable in the runtime environment.
var Helsinki = loadHelsinki()

func loadHelsinki() *time.Location {
	loc, err := time.LoadLocation("Europe/Helsinki")
	if err != nil {
		return time.UTC
	}
	return loc
}

var timestampLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
}

// parseTimestamp tries each known Wilma TimeStamp layout in turn. It never
// errors: on failure it returns the zero time, and callers keep the raw
// string alongside so no data is lost.
func parseTimestamp(raw string) time.Time {
	for _, layout := range timestampLayouts {
		if t, err := time.ParseInLocation(layout, raw, Helsinki); err == nil {
			return t
		}
	}
	return time.Time{}
}

// listMessage mirrors one entry of GET <prefix>/messages/index_json.
type listMessage struct {
	ID                 int64  `json:"Id"`
	Subject            string `json:"Subject"`
	TimeStamp          string `json:"TimeStamp"`
	Folder             string `json:"Folder"`
	SenderID           int64  `json:"SenderId"`
	SenderType         int    `json:"SenderType"`
	Sender             string `json:"Sender"`
	SenderStudentName  string `json:"SenderStudentName"`
	SenderGuardianName string `json:"SenderGuardianName"`
	Status             int    `json:"Status"`
	Replies            int    `json:"Replies"`
}

type listResponse struct {
	Messages []listMessage `json:"Messages"`
}

// messageDetail holds what's scraped from the server-rendered
// GET <prefix>/messages/<id> HTML page — this Wilma version has no JSON
// detail endpoint.
type messageDetail struct {
	ContentHtml string
	Recipients  []string
}

// Message is the normalized record wilmabridge emits, one per line of
// NDJSON on stdout.
type Message struct {
	Child      string    `json:"child"`
	Role       string    `json:"role"`
	ID         int64     `json:"id"`
	Subject    string    `json:"subject"`
	Sender     string    `json:"sender"`
	SenderID   int64     `json:"sender_id"`
	Folder     string    `json:"folder"`
	SentAt     time.Time `json:"sent_at,omitempty"`
	SentAtRaw  string    `json:"sent_at_raw"`
	Unread     bool      `json:"unread"`
	Replies    int       `json:"replies"`
	Recipients []string  `json:"recipients,omitempty"`
	BodyText   string    `json:"body_text,omitempty"`
	BodyHTML   string    `json:"body_html,omitempty"`
	BodyError  string    `json:"body_error,omitempty"`
	URL        string    `json:"url"`
}

// MarshalJSON customizes SentAt so a zero time (unparseable timestamp)
// serializes as omitted rather than "0001-01-01T00:00:00Z".
func (m Message) MarshalJSON() ([]byte, error) {
	type alias Message
	aux := struct {
		SentAt string `json:"sent_at,omitempty"`
		alias
	}{alias: alias(m)}
	if !m.SentAt.IsZero() {
		aux.SentAt = m.SentAt.Format(time.RFC3339)
	}
	return json.Marshal(aux)
}

// ListFolder fetches the message list for a role in the given folder.
// folder is "" (or "inbox") for the default inbox, or one of
// all/outbox/archive/drafts. Mirrors the URL the Wilma web UI itself
// builds: <prefix>/messages/list[/<folder>], folder omitted for inbox.
func (c *Client) ListFolder(prefix, folder string) ([]listMessage, error) {
	suffix := "/messages/list"
	if folder != "" && folder != "inbox" {
		suffix += "/" + folder
	}
	var lr listResponse
	if err := c.GetJSON(prefix, suffix, &lr); err != nil {
		return nil, err
	}
	return lr.Messages, nil
}

var (
	ckeditorDivPattern    = regexp.MustCompile(`<div class="ckeditor hidden">`)
	recipientsCellPattern = regexp.MustCompile(`<div id="recipients-cell"[^>]*>`)
	recipientNamePattern  = regexp.MustCompile(`(?s)<a[^>]*>(.*?)</a>`)
	divOpenClosePattern   = regexp.MustCompile(`(?i)<div\b|</div\s*>`)
)

// extractBalancedDiv returns the inner HTML of the <div ...> that starts at
// openTagEnd (the byte offset right after the opening tag's ">"), by
// counting nested <div>/</div> until the matching close. Needed because a
// message body can itself contain nested <div> elements, so a naive
// non-greedy regex would truncate at the first inner close.
func extractBalancedDiv(html string, openTagEnd int) (string, bool) {
	depth := 1
	for _, m := range divOpenClosePattern.FindAllStringIndex(html[openTagEnd:], -1) {
		tag := html[openTagEnd+m[0] : openTagEnd+m[1]]
		if strings.HasPrefix(tag, "</") {
			depth--
			if depth == 0 {
				return html[openTagEnd : openTagEnd+m[0]], true
			}
		} else {
			depth++
		}
	}
	return "", false
}

func extractContentHTML(html string) (string, bool) {
	loc := ckeditorDivPattern.FindStringIndex(html)
	if loc == nil {
		return "", false
	}
	return extractBalancedDiv(html, loc[1])
}

func extractRecipients(html string) []string {
	loc := recipientsCellPattern.FindStringIndex(html)
	if loc == nil {
		return nil
	}
	cell, ok := extractBalancedDiv(html, loc[1])
	if !ok {
		return nil
	}
	var names []string
	for _, m := range recipientNamePattern.FindAllStringSubmatch(cell, -1) {
		if name := strings.TrimSpace(htmlToText(m[1])); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// loginPagePattern matches the login form Wilma renders when a session has
// expired and a role-scoped GET gets redirected to /login instead of the
// page that was asked for.
var loginPagePattern = regexp.MustCompile(`(?i)<input[^>]+name="Password"`)

// FetchMessage fetches the server-rendered detail page for one message and
// scrapes its body and (when Wilma discloses them) recipients from it.
//
// Unlike GetJSON, this does not go through GetHTML: the message detail page
// has no JSON error envelope to check, so an expired session here shows up
// only as a redirect to /login. FetchMessage checks for that explicitly and
// returns ErrSessionExpired so callers (notably an automatic poller) can
// tell "session died" apart from "Wilma changed its page layout".
func (c *Client) FetchMessage(prefix string, id int64) (*messageDetail, error) {
	resp, err := c.get(prefix + fmt.Sprintf("/messages/%d", id))
	if err != nil {
		return nil, err
	}
	body, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	html := string(body)

	if resp.Request.URL.Path == "/login" || loginPagePattern.MatchString(html) {
		return nil, fmt.Errorf("wilma: message %d: %w (redirected to login while fetching message)", id, ErrSessionExpired)
	}

	content, ok := extractContentHTML(html)
	if !ok {
		return nil, fmt.Errorf("wilma: message %d: could not find message body in response (session expired, or page layout changed?)", id)
	}
	return &messageDetail{
		ContentHtml: content,
		Recipients:  extractRecipients(html),
	}, nil
}

// ToMessages builds normalized records from a role's message list, before
// any body fetch.
func ToMessages(prefix, childName, host string, list []listMessage) []Message {
	msgs := make([]Message, 0, len(list))
	for _, lm := range list {
		msgs = append(msgs, toMessage(prefix, childName, host, lm))
	}
	return msgs
}

// FillBody fetches the full message detail and populates BodyText/BodyHTML
// (and Recipients) on msg in place.
func FillBody(c *Client, prefix string, msg *Message) error {
	detail, err := c.FetchMessage(prefix, msg.ID)
	if err != nil {
		return err
	}
	msg.BodyHTML = detail.ContentHtml
	msg.BodyText = htmlToText(detail.ContentHtml)
	msg.Recipients = detail.Recipients
	return nil
}

// toMessage builds the normalized record from a list entry, before any body
// fetch.
func toMessage(prefix, childName string, host string, lm listMessage) Message {
	sender := lm.Sender
	if sender == "" {
		sender = lm.SenderGuardianName
	}
	folder := lm.Folder
	if folder == "" {
		folder = "inbox"
	}
	return Message{
		Child:     childName,
		Role:      prefix,
		ID:        lm.ID,
		Subject:   lm.Subject,
		Sender:    sender,
		SenderID:  lm.SenderID,
		Folder:    folder,
		SentAt:    parseTimestamp(lm.TimeStamp),
		SentAtRaw: lm.TimeStamp,
		Unread:    lm.Status == 1,
		Replies:   lm.Replies,
		URL:       fmt.Sprintf("https://%s%s/messages/%d", host, prefix, lm.ID),
	}
}
