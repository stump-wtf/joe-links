// Governing: SPEC-0017 REQ "LLM Provider Abstraction", REQ "Default Prompt Template"
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/config"
)

func anthropicEnvelope(text string) string {
	b, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
	return string(b)
}

// Governing: SPEC-0017 REQ "Default Prompt Template" scenario "LLM returns malformed JSON" (#169)
// The anthropic suggester must also surface raw text via *MalformedResponseError.
func TestAnthropic_MalformedContent_ReturnsRawError(t *testing.T) {
	const raw = "<<<not json>>>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(anthropicEnvelope(raw)))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.LLM.Provider = "anthropic"
	cfg.LLM.APIKey = "sk"
	cfg.LLM.Model = "test-model"
	sg := newAnthropicSuggester(cfg)
	// Point the suggester at the fake server.
	sg.client = srv.Client()
	sg.apiURL = srv.URL

	_, err := sg.Suggest(context.Background(), SuggestRequest{URL: "https://example.com"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var mre *MalformedResponseError
	if !errors.As(err, &mre) {
		t.Fatalf("error type = %T, want *MalformedResponseError", err)
	}
	if mre.Raw != raw {
		t.Errorf("Raw = %q, want %q", mre.Raw, raw)
	}
}

// Governing: SPEC-0017 REQ "Suggest API Endpoint" scenario "LLM call fails" (#201)
// A hung provider must not pin the caller: Suggest must fail with
// context.DeadlineExceeded once the per-call timeout elapses.
func TestAnthropic_HungProvider_TimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // block far longer than the suggester timeout
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close (LIFO)

	cfg := &config.Config{}
	cfg.LLM.Provider = "anthropic"
	cfg.LLM.APIKey = "sk"
	cfg.LLM.Model = "test-model"
	sg := newAnthropicSuggester(cfg)
	sg.client = srv.Client()
	sg.apiURL = srv.URL
	sg.timeout = 50 * time.Millisecond

	start := time.Now()
	_, err := sg.Suggest(context.Background(), SuggestRequest{URL: "https://example.com"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Suggest took %v; want it bounded by the ~50ms timeout", elapsed)
	}
}
