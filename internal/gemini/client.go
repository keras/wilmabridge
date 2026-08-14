// Package gemini is a thin, generic client for Google AI Studio's Gemini
// Interactions API (https://generativelanguage.googleapis.com/v1beta/interactions).
// It knows nothing about Wilma, school messages, or Finnish dates — it just
// sends a schema-constrained generation request and hands back the model's
// raw JSON text. Domain logic lives in internal/extract.
//
// Verified live (2026-08-13) against gemini-3.5-flash-lite: the response is
// NOT the older generateContent shape (candidates[].content.parts[]). It's
// steps[], where a "thought" step typically precedes the "model_output" step
// whose content[0].text carries the requested JSON as a string — so parsing
// is a double-decode: find the model_output step, then json.Unmarshal its
// text.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"
)

const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta/interactions"

// RawExchange captures exactly what was sent and received, for -v logging
// and for recording new test fixtures from live traffic.
type RawExchange struct {
	Request    []byte
	Response   []byte
	StatusCode int
	Attempts   int
}

// Client is a configured Interactions API caller.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
	maxRetries int
	verbose    func(format string, args ...any)
}

// Option configures a Client.
type Option func(*Client)

func WithBaseURL(url string) Option {
	return func(c *Client) {
		if url != "" {
			c.baseURL = url
		}
	}
}

func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

func WithVerbose(f func(format string, args ...any)) Option {
	return func(c *Client) {
		if f != nil {
			c.verbose = f
		}
	}
}

func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// NewClient builds a Client. apiKey and model are required.
func NewClient(apiKey, model string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("gemini: empty API key")
	}
	if model == "" {
		return nil, errors.New("gemini: empty model")
	}
	c := &Client{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		baseURL:    DefaultBaseURL,
		apiKey:     apiKey,
		model:      model,
		maxRetries: 3,
		verbose:    func(string, ...any) {},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

type requestBody struct {
	Model          string         `json:"model"`
	Input          string         `json:"input"`
	ResponseFormat responseFormat `json:"response_format"`
}

type responseFormat struct {
	Type     string          `json:"type"`
	MimeType string          `json:"mime_type"`
	Schema   json.RawMessage `json:"schema"`
}

type interactionResponse struct {
	Status string `json:"status"`
	Usage  struct {
		TotalTokens       int `json:"total_tokens"`
		TotalInputTokens  int `json:"total_input_tokens"`
		TotalOutputTokens int `json:"total_output_tokens"`
	} `json:"usage"`
	Steps []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"steps"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// APIError wraps a non-2xx HTTP response from the Interactions API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gemini: HTTP %d: %s", e.StatusCode, e.Body)
}

func retryable(statusCode int) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	return statusCode >= 500 && statusCode <= 599
}

// Generate sends input plus a JSON schema and returns the model's raw JSON
// text (the schema-shaped payload, not yet unmarshaled into a caller type)
// along with the full raw exchange for logging/fixture recording. Retries
// on 429/5xx with exponential backoff and jitter; a 4xx other than 429 is
// assumed to be a bug in the request and is never retried.
func (c *Client) Generate(ctx context.Context, input string, schema json.RawMessage) (text string, exchange RawExchange, err error) {
	reqBody := requestBody{
		Model: c.model,
		Input: input,
		ResponseFormat: responseFormat{
			Type:     "text",
			MimeType: "application/json",
			Schema:   schema,
		},
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", RawExchange{}, fmt.Errorf("gemini: encoding request: %w", err)
	}
	exchange.Request = reqJSON

	c.verbose("gemini: model=%s prompt (%d bytes):\n%s", c.model, len(input), truncateForLog(input, verboseLogLimit))

	var lastErr error
	for attempt := 1; attempt <= c.maxRetries; attempt++ {
		exchange.Attempts = attempt
		if attempt > 1 {
			delay := backoff(attempt)
			c.verbose("gemini: retry %d/%d after %s (%v)", attempt, c.maxRetries, delay, lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return "", exchange, ctx.Err()
			}
		}

		text, respBody, status, callErr := c.doOnce(ctx, reqJSON)
		exchange.Response = respBody
		exchange.StatusCode = status

		if callErr == nil {
			return text, exchange, nil
		}
		lastErr = callErr

		var apiErr *APIError
		if errors.As(callErr, &apiErr) && !retryable(apiErr.StatusCode) {
			return "", exchange, callErr
		}
	}
	return "", exchange, fmt.Errorf("gemini: giving up after %d attempts: %w", c.maxRetries, lastErr)
}

func (c *Client) doOnce(ctx context.Context, reqJSON []byte) (text string, respBody []byte, statusCode int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(reqJSON))
	if err != nil {
		return "", nil, 0, fmt.Errorf("gemini: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	c.verbose("POST %s", c.baseURL)
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return "", nil, 0, fmt.Errorf("gemini: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", nil, resp.StatusCode, fmt.Errorf("gemini: reading response: %w", err)
	}
	c.verbose("  -> %s (%d bytes, %s)", resp.Status, len(respBody), elapsed.Round(time.Millisecond))

	if resp.StatusCode != http.StatusOK {
		c.verbose("  error body:\n%s", truncateForLog(string(respBody), verboseLogLimit))
		return "", respBody, resp.StatusCode, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var ir interactionResponse
	if err := json.Unmarshal(respBody, &ir); err != nil {
		return "", respBody, resp.StatusCode, fmt.Errorf("gemini: decoding response envelope: %w", err)
	}
	if ir.Error != nil {
		return "", respBody, resp.StatusCode, fmt.Errorf("gemini: interaction error: %s", ir.Error.Message)
	}
	c.verbose("  usage: %d tokens total (%d in / %d out)", ir.Usage.TotalTokens, ir.Usage.TotalInputTokens, ir.Usage.TotalOutputTokens)
	for _, step := range ir.Steps {
		if step.Type != "model_output" {
			continue
		}
		if len(step.Content) == 0 {
			return "", respBody, resp.StatusCode, errors.New("gemini: model_output step has no content")
		}
		text := step.Content[0].Text
		c.verbose("  model_output (%d bytes):\n%s", len(text), truncateForLog(text, verboseLogLimit))
		return text, respBody, resp.StatusCode, nil
	}
	return "", respBody, resp.StatusCode, fmt.Errorf("gemini: no model_output step in response (status=%q, %d steps)", ir.Status, len(ir.Steps))
}

// verboseLogLimit caps how much of a prompt/response -v prints per call.
// Real school messages are small (a page of text, a few hundred tokens);
// this exists as a safety net against a pathological input, not a routine
// truncation — raise it if a genuinely long message gets cut off mid-debug.
const verboseLogLimit = 12000

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%s\n... (truncated, %d bytes total)", s[:max], len(s))
}

// backoff returns capped exponential backoff with jitter: attempt 2 -> ~500ms,
// attempt 3 -> ~1s, attempt 4 -> ~2s, capped at 8s.
func backoff(attempt int) time.Duration {
	base := 250 * time.Millisecond
	d := base << uint(attempt-1)
	const cap = 8 * time.Second
	if d > cap {
		d = cap
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 2))
	return d + jitter
}
