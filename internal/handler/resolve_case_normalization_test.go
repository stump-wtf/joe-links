// Tests for SPEC-0019 REQ "Case-Insensitive Slug Resolution" and REQ "Slug
// Normalization Forgiveness" — one test per spec scenario, named after it
// (issue #267).
// Governing: SPEC-0019, ADR-0019
package handler

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/store"
)

// Scenario: Mixed-Case Slug Resolves
// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution"
func TestResolve_MixedCaseSlugResolves(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "jira", "https://example.atlassian.net/browse")

	w := env.resolve(t, "/JIRA")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if loc != "https://example.atlassian.net/browse" {
		t.Errorf("Location = %q, want %q", loc, "https://example.atlassian.net/browse")
	}
}

// Scenario: Mixed-Case Slug Resolves — "the server MUST apply the link's
// visibility rules (SPEC-0010)": a secure link found via a case-folded lookup
// still sends anonymous visitors to login, not to the destination.
// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution"
func TestResolve_MixedCaseSlugResolves_AppliesVisibility(t *testing.T) {
	env := newResolveTestEnv(t)
	if _, err := env.ls.Create(context.Background(), "secure-docs", "https://internal.example.com", env.userID, "", "", "secure"); err != nil {
		t.Fatalf("seed secure link: %v", err)
	}

	w := env.resolve(t, "/SECURE-DOCS")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/auth/login") {
		t.Errorf("Location = %q, want login redirect", loc)
	}
}

// Scenario: Mixed-Case Prefix with Variable Link Preserves Argument Case
// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution"
func TestResolve_MixedCasePrefixWithVariableLinkPreservesArgumentCase(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "jira", "https://example.atlassian.net/browse/$ticket")

	w := env.resolve(t, "/Jira/PROJ-123")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if loc != "https://example.atlassian.net/browse/PROJ-123" {
		t.Errorf("Location = %q, want %q (argument case must be preserved)", loc, "https://example.atlassian.net/browse/PROJ-123")
	}
}

// Case-folding applies uniformly to every prefix candidate, static links
// included (REQ "Case-Insensitive Slug Resolution": "exact-match lookup and
// every prefix candidate").
// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution"
func TestResolve_MixedCasePrefixStaticLink(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "docs", "https://docs.example.com")

	w := env.resolve(t, "/DOCS/Anything/Here")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if loc != "https://docs.example.com" {
		t.Errorf("Location = %q, want %q", loc, "https://docs.example.com")
	}
}

// Scenario: Keyword Routing Precedence Unchanged
// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution"
func TestResolve_KeywordRoutingPrecedenceUnchanged(t *testing.T) {
	env := newResolveTestEnv(t)

	// Path-based: the first path segment matches a registered keyword, so
	// keyword routing must win before any slug lookup — even though the "gh"
	// variable link would otherwise prefix-match the same path.
	env.seedKeyword(t, "gh", "https://github.com/{slug}", "GitHub shortcut")
	env.seedLink(t, "gh", "https://linkstore.example.com/$user")

	w := env.resolve(t, "/gh/joestump")
	if w.Code != http.StatusFound {
		t.Fatalf("path keyword: status = %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "https://github.com/joestump" {
		t.Errorf("path keyword: Location = %q, want %q", loc, "https://github.com/joestump")
	}

	// Host-based: the request host matches a registered keyword, so host
	// routing must win before the "slack" link is ever looked up.
	env.seedKeyword(t, "go", "https://go.example.com/{slug}", "")
	env.seedLink(t, "slack", "https://slack.example.com")

	w = env.resolveWithHost(t, "/slack", "go")
	if w.Code != http.StatusFound {
		t.Fatalf("host keyword: status = %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "https://go.example.com/slack" {
		t.Errorf("host keyword: Location = %q, want %q", loc, "https://go.example.com/slack")
	}
}

// Scenario: Uppercase Slug Still Rejected at Creation
// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution"
func TestResolve_UppercaseSlugStillRejectedAtCreation(t *testing.T) {
	env := newResolveTestEnv(t)

	if _, err := env.ls.Create(context.Background(), "Jira", "https://example.com", env.userID, "", "", ""); !errors.Is(err, store.ErrSlugInvalid) {
		t.Errorf("Create(Jira) error = %v, want ErrSlugInvalid", err)
	}
	if _, err := env.ls.CreateFull(context.Background(), "Jira", "https://example.com", env.userID, "", "", "", nil, nil, nil, ""); !errors.Is(err, store.ErrSlugInvalid) {
		t.Errorf("CreateFull(Jira) error = %v, want ErrSlugInvalid", err)
	}
}

// Scenario: Underscore Forgiven
// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
func TestResolve_UnderscoreForgiven(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "standup-notes", "https://notes.example.com")

	w := env.resolve(t, "/standup_notes")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if loc != "https://notes.example.com" {
		t.Errorf("Location = %q, want %q", loc, "https://notes.example.com")
	}
}

// Scenario: Trailing Punctuation Forgiven — every character in the spec's
// forgiven set (`.`, `,`, `;`, `:`, `!`, `?`, `)`), plus a stacked `).` as
// pasted inside parentheses at the end of a sentence.
// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
func TestResolve_TrailingPunctuationForgiven(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "jira", "https://jira.example.com")

	// "?" must be percent-encoded in the request target or it would start the
	// query string; the handler still sees the decoded "jira?" path.
	for _, path := range []string{"/jira.", "/jira,", "/jira;", "/jira:", "/jira!", "/jira%3F", "/jira)", "/jira)."} {
		t.Run(path, func(t *testing.T) {
			w := env.resolve(t, path)
			if w.Code != http.StatusFound {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
			}
			loc := w.Header().Get("Location")
			if loc != "https://jira.example.com" {
				t.Errorf("Location = %q, want %q", loc, "https://jira.example.com")
			}
		})
	}
}

// Normalized matches pass through the same visibility enforcement as exact
// matches (REQ "Slug Normalization Forgiveness": "subject to the link's
// visibility rules"): a secure link reached via underscore forgiveness still
// sends anonymous visitors to login.
// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
func TestResolve_NormalizedMatchAppliesVisibility(t *testing.T) {
	env := newResolveTestEnv(t)
	if _, err := env.ls.Create(context.Background(), "standup-notes", "https://notes.example.com", env.userID, "", "", "secure"); err != nil {
		t.Fatalf("seed secure link: %v", err)
	}

	w := env.resolve(t, "/standup_notes")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/auth/login") {
		t.Errorf("Location = %q, want login redirect", loc)
	}
}

// Normalization retries run on the case-folded path, so a mixed-case,
// underscored, punctuation-suffixed paste still resolves.
// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution", REQ "Slug Normalization Forgiveness"
func TestResolve_NormalizationAppliesAfterCaseFold(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "standup-notes", "https://notes.example.com")

	w := env.resolve(t, "/Standup_Notes.")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if loc != "https://notes.example.com" {
		t.Errorf("Location = %q, want %q", loc, "https://notes.example.com")
	}
}

// Normalization is a retry, not a rewrite: an exact (case-folded) match always
// wins over any normalized candidate.
// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
func TestResolve_ExactMatchBeatsNormalization(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "standup-notes", "https://hyphen.example.com")
	env.seedRawLink(t, "standup_notes", "https://underscore.example.com")

	w := env.resolve(t, "/standup_notes")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if loc != "https://underscore.example.com" {
		t.Errorf("Location = %q, want %q (exact match must win)", loc, "https://underscore.example.com")
	}
}

// Forgiveness applies only to the whole-path exact lookup, never to prefix
// candidates (the spec's MAY is scoped "before falling through to prefix
// matching"): a typo'd prefix of a variable link still 404s. Pins the
// spec-permitted asymmetry — and that skipping candidate generation for
// multi-segment paths changes no behavior.
// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
func TestResolve_ForgivenessDoesNotExtendToPrefixCandidates(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "standup-notes", "https://notes.example.com/$date")

	w := env.resolve(t, "/standup_notes/2026-07-18")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (forgiveness must not apply to prefix candidates)", w.Code, http.StatusNotFound)
	}
}

// Pin the candidate generation: order, dedupe, and the no-candidate cases.
// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
func TestNormalizationCandidates(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"standup_notes", []string{"standup-notes"}},
		{"jira.", []string{"jira"}},
		{"standup_notes.", []string{"standup-notes.", "standup_notes", "standup-notes"}},
		{"jira", nil}, // nothing to forgive
		{"...", nil},  // trims to empty — never looked up
		{"a_b.", []string{"a-b.", "a_b", "a-b"}},
		// Slugs cannot contain "/", so multi-segment paths generate no
		// candidates — every derived lookup would be a guaranteed miss.
		{"foo_bar/baz.", nil},
		{"a/b", nil},
	}
	for _, tc := range cases {
		if got := normalizationCandidates(tc.path); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("normalizationCandidates(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
