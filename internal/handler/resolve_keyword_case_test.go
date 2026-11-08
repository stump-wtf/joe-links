// Tests for SPEC-0019 REQ "Case-Folded Keyword Matching" (issue #284): the
// case of a URL's first segment must not change which resource it reaches, so
// keyword routing follows the same case rule the slug resolver already
// applies.
// Governing: SPEC-0019 REQ "Case-Folded Keyword Matching", ADR-0011, ADR-0027
package handler

import (
	"context"
	"net/http"
	"testing"
)

// Scenario: Case-Variant Keyword Path Reaches The Same Destination
// Governing: SPEC-0019 REQ "Case-Folded Keyword Matching"
func TestResolve_KeywordPathCaseVariantsReachSameDestination(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedKeyword(t, "gh", "https://github.com/{slug}", "GitHub shortcut")
	// A link owns the same name as the keyword: keyword routing wins for every
	// case variant, so the case of the first segment cannot pick a destination.
	env.seedLink(t, "gh", "https://linkstore.example.com/$user")

	for _, path := range []string{"/gh/joestump", "/GH/joestump", "/Gh/joestump"} {
		w := env.resolve(t, path)
		if w.Code != http.StatusFound {
			t.Fatalf("%s: status = %d, want %d", path, w.Code, http.StatusFound)
		}
		if loc := w.Header().Get("Location"); loc != "https://github.com/joestump" {
			t.Errorf("%s: Location = %q, want %q", path, loc, "https://github.com/joestump")
		}
	}
}

// Scenario: Case-Variant Keyword Host Routes Via Keyword
// Governing: SPEC-0019 REQ "Case-Folded Keyword Matching"
func TestResolve_KeywordHostCaseVariantsRouteViaKeyword(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedKeyword(t, "go", "https://go.example.com/{slug}", "")
	env.seedLink(t, "slack", "https://slack.example.com")

	for _, host := range []string{"go", "GO", "Go"} {
		w := env.resolveWithHost(t, "/slack", host)
		if w.Code != http.StatusFound {
			t.Fatalf("host %q: status = %d, want %d", host, w.Code, http.StatusFound)
		}
		if loc := w.Header().Get("Location"); loc != "https://go.example.com/slack" {
			t.Errorf("host %q: Location = %q, want %q", host, loc, "https://go.example.com/slack")
		}
	}
}

// Scenario: Keyword Name Stays A Creatable Slug — reservation was the rejected
// option, so a link may still take a keyword's name; the guarantee is that
// whichever case the first segment arrives in, the keyword handles it.
// Governing: SPEC-0019 REQ "Case-Folded Keyword Matching"
func TestResolve_KeywordNameStaysCreatableSlugAndRoutesToKeyword(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedKeyword(t, "wtf", "https://search.example.com/?q={slug}", "")

	if _, err := env.ls.Create(context.Background(), "wtf", "https://docs.example.com/$query", env.userID, "", "", ""); err != nil {
		t.Fatalf("Create(wtf) while keyword wtf exists: %v", err)
	}

	for _, path := range []string{"/wtf/nginx", "/WTF/nginx"} {
		w := env.resolve(t, path)
		if w.Code != http.StatusFound {
			t.Fatalf("%s: status = %d, want %d", path, w.Code, http.StatusFound)
		}
		if loc := w.Header().Get("Location"); loc != "https://search.example.com/?q=nginx" {
			t.Errorf("%s: Location = %q, want %q", path, loc, "https://search.example.com/?q=nginx")
		}
	}
}

// Scenario: Keyword Miss Falls Through To Slug Resolution — folding the
// keyword lookup must not make a non-keyword first segment resolve as one.
// Governing: SPEC-0019 REQ "Case-Folded Keyword Matching"
func TestResolve_KeywordLookupMissFallsThroughToSlugResolution(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedKeyword(t, "gh", "https://github.com/{slug}", "")
	env.seedLink(t, "docs", "https://docs.example.com")

	for _, path := range []string{"/docs", "/DOCS"} {
		w := env.resolve(t, path)
		if w.Code != http.StatusFound {
			t.Fatalf("%s: status = %d, want %d", path, w.Code, http.StatusFound)
		}
		if loc := w.Header().Get("Location"); loc != "https://docs.example.com" {
			t.Errorf("%s: Location = %q, want %q", path, loc, "https://docs.example.com")
		}
	}
}
