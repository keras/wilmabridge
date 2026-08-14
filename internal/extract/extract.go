package extract

import (
	"context"
	"encoding/json"
	"fmt"

	"wilmabridge/internal/gemini"
	"wilmabridge/internal/wilma"
)

// ExtractMessage sends one message's body through Gemini and returns the
// validated events found in it (zero events is a normal, common result —
// most school messages carry no actionable date). The gemini.RawExchange
// is always returned, even on error, so callers can log or record it.
func ExtractMessage(ctx context.Context, client *gemini.Client, msg wilma.Message) ([]Event, gemini.RawExchange, error) {
	input := BuildInput(msg.Subject, msg.BodyText)

	text, exchange, err := client.Generate(ctx, input, responseSchema)
	if err != nil {
		return nil, exchange, fmt.Errorf("extract: message %d: %w", msg.ID, err)
	}

	var cr candidatesResponse
	if err := json.Unmarshal([]byte(text), &cr); err != nil {
		return nil, exchange, fmt.Errorf("extract: message %d: decoding model output: %w", msg.ID, err)
	}

	src := Source{
		WilmaID:        msg.ID,
		Child:          msg.Child,
		Role:           msg.Role,
		MessageSubject: msg.Subject,
		MessageURL:     msg.URL,
		SentAt:         msg.SentAt,
	}
	events := make([]Event, 0, len(cr.Candidates))
	for _, c := range cr.Candidates {
		events = append(events, BuildEvent(src, c))
	}
	return events, exchange, nil
}
