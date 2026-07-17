// Reserved-slug convergence: the dashboard form and the live availability
// checker must apply the same exact-match reservation rule as the store,
// REST API, and MCP tools — no extra {prefix}-* rule in the UI (issue #204).
//
// Governing: SPEC-0001 REQ "Short Link Resolution"
// Governing: SPEC-0004 REQ "New Link Form" — live slug validation
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// TestLinksCreate_DashPrefixedSlugAllowed verifies the form no longer rejects
// dash-prefixed slugs like "u-foo": routes are path-segmented, so /u/{...}
// and /u-foo are distinct and no prefix rule applies (#204).
func TestLinksCreate_DashPrefixedSlugAllowed(t *testing.T) {
	env := newLinkFormEnv(t)

	w := env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
		"slug": {"u-foo"},
		"url":  {"https://example.com/u-foo"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (redirect on success); body: %s", w.Code, http.StatusSeeOther, w.Body.String())
	}
	link, err := env.ls.GetBySlug(context.Background(), "u-foo")
	if err != nil {
		t.Fatalf("GetBySlug(u-foo) after create = %v, want link", err)
	}
	if link.Slug != "u-foo" {
		t.Errorf("slug = %q, want %q", link.Slug, "u-foo")
	}
}

// TestLinksCreate_ReservedSlugRejected verifies the form rejects an
// exact-match reserved slug via the store's single validation entry point,
// with an error that names the reserved set.
func TestLinksCreate_ReservedSlugRejected(t *testing.T) {
	env := newLinkFormEnv(t)

	w := env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
		"slug": {"mcp"},
		"url":  {"https://example.com/mcp"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (form re-render with error)", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !strings.Contains(body, "reserved") {
		t.Errorf("form error missing reserved-slug message: %s", body)
	}
	if _, err := env.ls.GetBySlug(context.Background(), "mcp"); err == nil {
		t.Error("reserved slug was created; want rejection")
	}
}

// validateSlugEnv wires a LinksHandler for exercising the live availability
// checker endpoint directly.
func validateSlugEnv(t *testing.T) *LinksHandler {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	ks := store.NewKeywordStore(db)
	return NewLinksHandler(ls, owns, us, ks)
}

// TestValidateSlug_ReservedAndDashPrefixed verifies the live checker agrees
// with the store rule: reserved words are flagged, dash-prefixed slugs are
// available (#204).
func TestValidateSlug_ReservedAndDashPrefixed(t *testing.T) {
	h := validateSlugEnv(t)

	check := func(slug string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/dashboard/links/validate-slug?slug="+url.QueryEscape(slug), nil)
		w := httptest.NewRecorder()
		h.ValidateSlug(w, req)
		return w.Body.String()
	}

	// Every reserved word must be reported as reserved, not available.
	for _, slug := range store.ReservedSlugs() {
		body := check(slug)
		if !strings.Contains(body, "reserved") {
			t.Errorf("ValidateSlug(%q) = %q, want reserved-slug error", slug, body)
		}
		if strings.Contains(body, "Available!") {
			t.Errorf("ValidateSlug(%q) reported available; want reserved", slug)
		}
	}

	// Dash-prefixed slugs are unclaimed and valid — available.
	for _, slug := range []string{"u-foo", "links-roundup", "api-test"} {
		if body := check(slug); !strings.Contains(body, "Available!") {
			t.Errorf("ValidateSlug(%q) = %q, want available", slug, body)
		}
	}
}
