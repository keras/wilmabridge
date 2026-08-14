package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadFixture reads a testdata/*.json fixture from internal/extract (shared
// with that package's tests) shaped {"request": ..., "response": ...}.
func loadFixture(t *testing.T, name string) (reqBody []byte, respBody []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "extract", "testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	var f struct {
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	return f.Request, f.Response
}

func TestGenerate_RequestShape(t *testing.T) {
	var gotBody []byte
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"completed","steps":[{"type":"thought"},{"type":"model_output","content":[{"type":"text","text":"{\"candidates\":[]}"}]}]}`))
	}))
	defer srv.Close()

	client, err := NewClient("test-key", "gemini-3.5-flash-lite", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	text, exchange, err := client.Generate(context.Background(), "hello", json.RawMessage(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if text != `{"candidates":[]}` {
		t.Errorf("text = %q", text)
	}
	if exchange.StatusCode != 200 || exchange.Attempts != 1 {
		t.Errorf("exchange = %+v", exchange)
	}
	if gotHeader.Get("x-goog-api-key") != "test-key" {
		t.Errorf("api key header = %q", gotHeader.Get("x-goog-api-key"))
	}

	var sent requestBody
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("server-side decode of sent body: %v", err)
	}
	if sent.Model != "gemini-3.5-flash-lite" || sent.Input != "hello" {
		t.Errorf("sent = %+v", sent)
	}
	if sent.ResponseFormat.Type != "text" || sent.ResponseFormat.MimeType != "application/json" {
		t.Errorf("response_format = %+v", sent.ResponseFormat)
	}
}

// TestGenerate_RealFixtures replays the genuine captured Gemini responses
// (see internal/extract/testdata/README.md) through the client's parser,
// proving it handles the real steps[] shape — including the thought step
// that precedes model_output — without needing live network access.
func TestGenerate_RealFixtures(t *testing.T) {
	cases := []struct {
		fixture      string
		wantContains string
	}{
		{"monthly_letter.json", `"kind": "exam"`},
		{"recurrence.json", `"count": 4`},
		{"deadline.json", `"kind": "deadline"`},
		{"no_date.json", `"candidates": []`},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			_, respBody := loadFixture(t, tc.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(respBody)
			}))
			defer srv.Close()

			client, err := NewClient("test-key", "gemini-3.5-flash-lite", WithBaseURL(srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			text, _, err := client.Generate(context.Background(), "irrelevant", json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if !strings.Contains(text, tc.wantContains) {
				t.Errorf("text = %s\nwant substring %q", text, tc.wantContains)
			}
			// Must be valid, directly unmarshalable JSON — this is the contract
			// internal/extract relies on.
			var parsed map[string]any
			if err := json.Unmarshal([]byte(text), &parsed); err != nil {
				t.Errorf("model_output text is not valid JSON: %v\ntext: %s", err, text)
			}
		})
	}
}

func TestGenerate_RetriesOn429ThenSucceeds(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Write([]byte(`{"status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"{\"ok\":true}"}]}]}`))
	}))
	defer srv.Close()

	client, err := NewClient("k", "m", WithBaseURL(srv.URL), WithMaxRetries(5))
	if err != nil {
		t.Fatal(err)
	}
	text, exchange, err := client.Generate(context.Background(), "x", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if exchange.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", exchange.Attempts)
	}
	if text != `{"ok":true}` {
		t.Errorf("text = %q", text)
	}
}

func TestGenerate_VerboseLogsPromptUsageAndRawOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"completed","usage":{"total_tokens":99,"total_input_tokens":80,"total_output_tokens":19},"steps":[{"type":"model_output","content":[{"type":"text","text":"{\"candidates\":[]}"}]}]}`))
	}))
	defer srv.Close()

	var lines []string
	verbose := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	client, err := NewClient("k", "m", WithBaseURL(srv.URL), WithVerbose(verbose))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Generate(context.Background(), "tämä on kehote", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"tämä on kehote",    // the prompt itself, not just its length
		"99 tokens total",   // usage
		`{"candidates":[]}`, // raw model output text
	} {
		if !strings.Contains(all, want) {
			t.Errorf("verbose log missing %q; got:\n%s", want, all)
		}
	}
}

func TestTruncateForLog(t *testing.T) {
	if got := truncateForLog("short", 100); got != "short" {
		t.Errorf("short string should pass through unchanged, got %q", got)
	}
	long := strings.Repeat("x", verboseLogLimit+500)
	got := truncateForLog(long, verboseLogLimit)
	if len(got) <= verboseLogLimit || len(got) >= len(long) {
		t.Errorf("truncated length = %d, want between %d and %d", len(got), verboseLogLimit, len(long))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncated output should say so: %q", got[len(got)-60:])
	}
}

func TestGenerate_DoesNotRetry400(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad schema"}}`))
	}))
	defer srv.Close()

	client, err := NewClient("k", "m", WithBaseURL(srv.URL), WithMaxRetries(5))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Generate(context.Background(), "x", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (400 must not retry)", calls)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Errorf("expected *APIError, got %T: %v", err, err)
	} else if apiErr.StatusCode != 400 {
		t.Errorf("status = %d", apiErr.StatusCode)
	}
}
