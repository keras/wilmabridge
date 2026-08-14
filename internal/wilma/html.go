package wilma

import (
	"html"
	"regexp"
	"strings"
)

var (
	scriptStylePattern  = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	brPattern           = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockClosePattern   = regexp.MustCompile(`(?i)</(p|div|h[1-6]|tr|table|blockquote)\s*>`)
	listItemPattern     = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	tagPattern          = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRunPattern     = regexp.MustCompile(`[ \t]+`)
	blankLineRunPattern = regexp.MustCompile(`\n{3,}`)
)

// htmlToText renders Wilma's ContentHtml message bodies down to plain text
// using only stdlib primitives: strip script/style blocks, turn line and
// block boundaries into newlines, drop remaining tags, unescape entities,
// and collapse whitespace. It is intentionally lossy — body_html always
// carries the original markup alongside it.
func htmlToText(in string) string {
	s := scriptStylePattern.ReplaceAllString(in, "")
	s = brPattern.ReplaceAllString(s, "\n")
	s = blockClosePattern.ReplaceAllString(s, "\n\n")
	s = listItemPattern.ReplaceAllString(s, "\n- ")
	s = tagPattern.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spaceRunPattern.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(strings.TrimLeft(l, " "), " \t\r")
	}
	s = strings.Join(lines, "\n")
	s = blankLineRunPattern.ReplaceAllString(s, "\n\n")
	return strings.Trim(s, "\n")
}
