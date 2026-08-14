package extract

import "strings"

// systemRules' first five bullet points are byte-identical to the text
// verified live against gemini-3.5-flash-lite across four real Finnish
// school messages (monthly letter with 4 dated lines, a 4-week recurrence,
// a deadline, and a message with no actionable date at all) — see
// internal/extract/testdata/README.md for the captured exchanges. Do not
// reword those casually: a "small wording tweak" is exactly the kind of
// change the prompt-regression test (extract_test.go, replaying these
// fixtures) exists to catch before it reaches a real inbox.
//
// The "audience" bullet (ver 2, see schema.go's ExtractVer comment) is new
// and, unlike the first five, NOT yet verified against a live response —
// appended rather than interleaved so the diff against the verified text
// stays obvious. Needs its own fixture (testdata/README.md) before it can
// be trusted the way the other rules are.
//
// ver 3 replaced the "items vain jos..." bullet with a "detail" bullet:
// location/items are no longer their own schema fields (see
// candidate.go's Candidate doc comment), so the model is told to fold that
// prose into detail instead of a structured field it no longer has.
//
// Two behaviors the design doc calls for — bilingual dedup and returning an
// empty candidates list when nothing is actionable — are deliberately NOT
// spelled out here as extra rules: the empty-list case was tested and the
// model did it correctly unprompted, and no bilingual fixture exists yet to
// justify adding an untested instruction. Add both only once a bilingual
// message is captured as a fixture to prove the rule actually helps.
const systemRules = `Olet kouluviestien jäsentäjä. Poimi viestistä päivämäärälliset tapahtumat.
SÄÄNNÖT:
- Älä koskaan päättele vuotta. Palauta päivämäärä täsmälleen kirjoitetussa muodossa.
- Älä koskaan luettele toistuvia tapahtumia; palauta recurrence {freq,count}.
- Palauta weekday_claim jos viestissä lukee viikonpäivä (ma/ti/ke/to/pe/la/su).
- Jos viestissä mainitaan paikka tai mukaan otettavat tavarat, kirjoita ne detail-kenttään.
- quote = sanatarkka lainaus viestistä.
- audience = "child" jos tapahtuma koskee oppilasta, "guardians" jos se on tarkoitettu vain huoltajille (esim. vanhempainilta).`

// BuildInput assembles the full model input for one message, matching the
// exact "<rules>\n\nVIESTI:\nOtsikko: <subject>\n<body>" layout used in the
// live-verified test calls.
func BuildInput(subject, body string) string {
	var b strings.Builder
	b.WriteString(systemRules)
	b.WriteString("\n\nVIESTI:\nOtsikko: ")
	b.WriteString(subject)
	b.WriteString("\n")
	b.WriteString(body)
	return b.String()
}
