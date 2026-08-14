package extract

import "encoding/json"

// ExtractVer identifies the prompt+schema combination that produced an
// Event. Bump it whenever schema.go or prompt.go changes meaningfully, so
// stored/logged events can be told apart from a future re-extraction.
const ExtractVer = 1

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
// four real messages (see testdata/README.md). The "description" strings
// and the time/location/link properties were added afterward for clarity
// and are NOT part of that verified run; they're plain optional string
// fields (no nested "required" of their own), so they don't carry the
// failure mode above, but confirm they behave as expected via the live
// smoke test before depending on them (see the plan's verification step 3).
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
          "detail": { "type": "string" },
          "date": {
            "type": "string",
            "description": "Päivämäärä TÄSMÄLLEEN viestissä kirjoitetussa muodossa, esim. \"4.3.\" tai \"10.10.\". Älä päättele tai lisää vuotta."
          },
          "weekday_claim": {
            "type": "string",
            "description": "Viestissä mainittu viikonpäivä lyhenteenä (ma/ti/ke/to/pe/la/su), jos sellainen on kirjoitettu päivämäärän yhteyteen."
          },
          "time": { "type": "string" },
          "location": { "type": "string" },
          "items": {
            "type": "array",
            "items": { "type": "string" },
            "description": "Mukaan otettavat tavarat, VAIN jos ne on nimenomaisesti mainittu. Älä päättele."
          },
          "link": {
            "type": "string",
            "description": "Viestissä mainittu URL, esim. kyselylomakkeen linkki, jos sellainen on."
          },
          "recurrence": {
            "type": "object",
            "description": "Täytä VAIN jos tapahtuma toistuu useana viikkona/päivänä. ÄLÄ koskaan luettele yksittäisiä toistokertoja erillisinä candidateina.",
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
          "confidence": {
            "type": "number",
            "description": "0-1. Huom: ei ole kalibroitu tarkasti, käytetään vain debug-tarkoituksiin."
          },
          "quote": {
            "type": "string",
            "description": "Sanatarkka lainaus viestistä, josta tämä tapahtuma poimittiin."
          }
        },
        "required": ["kind", "title", "date", "confidence", "quote"]
      }
    }
  },
  "required": ["candidates"]
}`)
