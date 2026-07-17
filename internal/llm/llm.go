// Governing: SPEC-0017 REQ "LLM Provider Abstraction", "LLM Provider Configuration", ADR-0017
package llm

import (
	"context"
	"fmt"
	"time"

	"github.com/joestump/joe-links/internal/config"
)

// Governing: SPEC-0017 REQ "Suggest API Endpoint" scenario "LLM call fails" (#201)
// Every provider call is bounded so a hung or blackholed provider cannot pin
// the calling request indefinitely: defaultSuggestTimeout caps a single
// Suggest call via context deadline, and defaultClientTimeout on the
// http.Client is defense in depth for contexts without a deadline.
const (
	defaultSuggestTimeout = 15 * time.Second
	defaultClientTimeout  = 30 * time.Second
)

// SuggestRequest is the input to the Suggester.
type SuggestRequest struct {
	URL         string
	Title       string
	Description string
}

// SuggestResponse is the structured suggestion returned by the LLM.
type SuggestResponse struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// Suggester generates link metadata suggestions via an LLM provider.
type Suggester interface {
	Suggest(ctx context.Context, req SuggestRequest) (*SuggestResponse, error)
}

// MalformedResponseError is returned when the model's content cannot be parsed
// as the expected suggestion JSON structure. It carries the raw model output so
// callers can log it for debugging.
// Governing: SPEC-0017 REQ "Default Prompt Template" scenario "LLM returns malformed JSON"
type MalformedResponseError struct {
	Raw string // raw text content returned by the model
	Err error  // underlying json.Unmarshal error
}

func (e *MalformedResponseError) Error() string {
	return fmt.Sprintf("decode suggestion JSON: %v", e.Err)
}

func (e *MalformedResponseError) Unwrap() error { return e.Err }

// New creates a Suggester based on the config. Returns nil when LLMProvider is
// unset, meaning LLM suggestions are disabled.
func New(cfg *config.Config) (Suggester, error) {
	switch cfg.LLM.Provider {
	case "":
		return nil, nil
	case "anthropic":
		return newAnthropicSuggester(cfg), nil
	case "openai", "openai-compatible":
		return newOpenAISuggester(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported LLM provider: %q", cfg.LLM.Provider)
	}
}
