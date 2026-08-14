package extract

import "encoding/json"

// ExtractVer identifies the prompt+schema combination that produced an
// Event. Bump it whenever schema.go or prompt.go changes meaningfully, so
// stored/logged events can be told apart from a future re-extraction.
//
// ver 2: added the "audience" field (child vs guardians) to responseSchema
// and prompt.go's rules.
//
// ver 3: removed "location" and "items" from responseSchema/Candidate/
// Event — that information now belongs in "detail" (or is already present
// verbatim in "quote") instead of its own structured field. Also: a
// recurring candidate is no longer stored as one Event carrying a
// {freq,count} hint — BuildEvent now expands it into one Event per
// occurrence, each independently dated and flagged (see validate.go). The
// model's request shape for recurrence itself (still {freq,count}) is
// unchanged; only what Go does with it changed.
//
// Every message extracted under an older ver is a candidate for
// `wilmabridge reextract`.
const ExtractVer = 3

// responseSchema is the JSON Schema handed to Gemini as response_format.schema.
//
// The single most important line in this file is every nested object's
// "required" list. Verified live: without recurrence.required = [freq,
// count], gemini-3.5-flash-lite silently dropped "count" from a message
// that said "neljänä perättäisenä viikkona" (four weeks) — it returned
// {"freq":"weekly"} with no count at all, no error, no low confidence
// signal (it reported confidence 1.0 on that very response). Adding the
// required list recovered {"freq":"weekly","count":4}. Any future nested
// object needs the same treatment, or its fields will be silently optional
// in practice regardless of what the prompt asks for.
//
// The structural shape here — every field name, the recurrence required
// list, the top-level required list — matches what was live-tested across
// four real messages (see testdata/README.md), MINUS "location" and
// "items", which those four captures did include but ver 3 removed (see
// the ExtractVer doc comment) — folded into "detail" instead. The
// "description" strings and the time/link properties were added afterward
// for clarity and are NOT part of that verified run; they're plain
// optional string fields (no nested "required" of their own), so they
// don't carry the failure mode above, but confirm they behave as expected
// via the live smoke test before depending on them (see the plan's
// verification step 3).
//
// "audience" (ver 2) is a top-level required field, exactly the treatment
// the recurrence.count incident above says a field needs to be trusted —
// but unlike everything else in this schema, it has NOT yet been verified
// against a live response: the four existing fixtures all predate it. Do
// not treat audience="guardians" as trustworthy in production until a real
// guardians-only message (e.g. a vanhempainilta notice) has been captured
// into testdata/ and its test passes. See testdata/README.md.
var responseSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "candidates": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "kind": {
            "type": "string",
            "enum": ["event", "deadline", "exam", "info"],
            "description": "event = tapahtuma; deadline = määräaika, johon mennessä pitää tehdä/vastata jotain; exam = koe; info = ei toiminnallista päivämäärää."
          },
          "title": { "type": "string" },
          "detail": {
            "type": "string",
            "description": "Tiedot tapahtumasta proosana, mukaanlukien paikka tai mukaan otettavat tavarat jos ne on mainittu viestissä."
          },
          "date": {
            "type": "string",
            "description": "Päivämäärä TÄSMÄLLEEN viestissä kirjoitetussa muodossa, esim. \"4.3.\" tai \"10.10.\". Älä päättele tai lisää vuotta."
          },
          "weekday_claim": {
            "type": "string",
            "description": "Viestissä mainittu viikonpäivä lyhenteenä (ma/ti/ke/to/pe/la/su), jos sellainen on kirjoitettu päivämäärän yhteyteen."
          },
          "time": { "type": "string" },
          "link": {
            "type": "string",
            "description": "Viestissä mainittu URL, esim. kyselylomakkeen linkki, jos sellainen on."
          },
          "recurrence": {
            "type": "object",
            "description": "Täytä VAIN jos tapahtuma toistuu useana viikkona/päivänä. ÄLÄ koskaan luettele yksittäisiä toistokertoja erillisinä kandidaatteina.",
            "properties": {
              "freq": {
                "type": "string",
                "enum": ["daily", "weekly", "biweekly", "monthly"]
              },
              "count": {
                "type": "integer",
                "description": "Toistokertojen kokonaismäärä, esim. 4 lauseessa 'neljänä perättäisenä viikkona'."
              }
            },
            "required": ["freq", "count"]
          },
          "audience": {
            "type": "string",
            "enum": ["child", "guardians"],
            "description": "Kenelle tapahtuma on suunnattu: \"child\" = koskee oppilasta (esim. retki, koe, uinti, koulupäivän aikataulu), \"guardians\" = tarkoitettu vain huoltajille, ei oppilaalle itselleen (esim. vanhempainilta, huoltajakysely, kouluterveydenhoitajan tiedote huoltajille)."
          },
          "confidence": {
            "type": "number",
            "description": "0-1. Huom: ei ole kalibroitu tarkasti, käytetään vain debug-tarkoituksiin."
          },
          "quote": {
            "type": "string",
            "description": "Sanatarkka lainaus viestistä, josta tämä tapahtuma poimittiin."
          }
        },
        "required": ["kind", "title", "date", "audience", "confidence", "quote"]
      }
    }
  },
  "required": ["candidates"]
}`)
