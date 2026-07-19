package handler

// Did-you-mean 404 suggestion tests (SPEC-0019 REQ "Did-You-Mean 404
// Suggestions"). Tests are named after the spec scenarios where one applies,
// so the spec↔test mapping is auditable; the remainder cover the REQ-level
// bounds (max 3, ordering, single-character gate), the lifecycle exclusion,
// the secure/private existence oracle, and output escaping.
//
// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions", ADR-0019

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// seedUser creates an additional user so tests can seed links the resolving
// viewer does not own.
func (e *resolveTestEnv) seedUser(t *testing.T, sub, email, name string) *store.User {
	t.Helper()
	us := store.NewUserStore(e.db)
	u, err := us.Upsert(context.Background(), "test", sub, email, name, "")
	if err != nil {
		t.Fatalf("seed user %q: %v", sub, err)
	}
	return u
}

// seedLinkVis creates a link with an explicit owner and visibility.
func (e *resolveTestEnv) seedLinkVis(t *testing.T, slug, ownerID, visibility string) *store.Link {
	t.Helper()
	l, err := e.ls.Create(context.Background(), slug, "https://example.com", ownerID, "", "", visibility)
	if err != nil {
		t.Fatalf("seed link %q: %v", slug, err)
	}
	return l
}

// resolveHTMX issues a GET through the resolve route with the HX-Request
// header set, as the given user (nil = anonymous).
func (e *resolveTestEnv) resolveHTMX(t *testing.T, path string, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/{slug}*", e.rh.Resolve)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("HX-Request", "true")
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertNoDidYouMean asserts the 404 body renders without a did-you-mean
// block at all.
func assertNoDidYouMean(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if strings.Contains(w.Body.String(), "Did you mean") {
		t.Error("body contains a did-you-mean block, want none")
	}
}

// TestLevenshtein pins the plain edit-distance semantics ADR-0019 chose: no
// transposition move, so a swap costs 2 — which still qualifies under the ≤2
// bound.
func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"jira", "jira", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"jria", "jira", 2}, // transposition = 2 in plain Levenshtein
		{"secrt", "secret", 1},
		{"kitten", "sitting", 3},
		{"zzzzz", "alpha", 5},
		{"héllo", "hello", 1}, // rune-wise, not byte-wise
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// Scenario: Typo Suggests Nearby Public Slug — an anonymous request for
// /jria with a public link jira must show "Did you mean {keyword}/jira?"
// above the Create CTA, linking to /jira.
func TestResolve404_DidYouMean_TypoSuggestsNearbyPublicSlug(t *testing.T) {
	env := newResolveTestEnv(t)
	other := env.seedUser(t, "sub-other", "other@example.com", "Other")
	env.seedLinkVis(t, "jira", other.ID, "public")

	w := env.resolveAs(t, "/jria", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Did you mean") {
		t.Fatalf("body missing did-you-mean block; body=%s", body)
	}
	if !strings.Contains(body, `href="/jira"`) {
		t.Error("suggestion must link to /jira so the resolver's visibility enforcement governs the redirect")
	}
	// httptest requests carry Host "example.com", so the advertised short
	// keyword is its first DNS label (SPEC-0005).
	if !strings.Contains(body, "example/jira") {
		t.Error("suggestion text must render as {keyword}/{slug}")
	}
	// Above the Create CTA (anonymous → the sign-in variant).
	dym := strings.Index(body, "Did you mean")
	cta := strings.Index(body, "Sign in to create this link")
	if cta == -1 {
		t.Fatal("Create CTA missing — did-you-mean must not disturb the CTA")
	}
	if dym > cta {
		t.Error("did-you-mean block must render above the Create CTA")
	}
}

// Scenario: Private Slug Existence Not Leaked to Anonymous Viewer — when the
// only slug within distance 2 is another user's private link, the anonymous
// 404 renders with no did-you-mean suggestions at all.
func TestResolve404_DidYouMean_PrivateSlugExistenceNotLeakedToAnonymousViewer(t *testing.T) {
	env := newResolveTestEnv(t)
	other := env.seedUser(t, "sub-other", "other@example.com", "Other")
	env.seedLinkVis(t, "secret", other.ID, "private")

	w := env.resolveAs(t, "/secrt", nil)
	assertNoDidYouMean(t, w)
	if strings.Contains(w.Body.String(), "secret") {
		t.Error("private slug leaked into the anonymous 404 body")
	}
}

// Scenario: Owner Sees Their Own Private Slug Suggested — an authenticated
// user who owns a private link secret gets "Did you mean {keyword}/secret?".
func TestResolve404_DidYouMean_OwnerSeesTheirOwnPrivateSlugSuggested(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLinkVis(t, "secret", env.userID, "private")

	w := env.resolveAs(t, "/secrt", env.user())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Did you mean") || !strings.Contains(body, `href="/secret"`) {
		t.Errorf("owner should be offered their own private slug; body=%s", body)
	}
}

// Scenario: Distance Bound Enforced — no visible slug within Levenshtein
// distance 2 means no did-you-mean block, with the Create CTA unchanged.
// "alpha" shares zzzzz's length, so it passes the SQL length window and is
// rejected by the in-Go distance bound specifically.
func TestResolve404_DidYouMean_DistanceBoundEnforced(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLinkVis(t, "alpha", env.userID, "public")

	w := env.resolveAs(t, "/zzzzz", env.user())
	assertNoDidYouMean(t, w)
	body := w.Body.String()
	if !strings.Contains(body, "Create this link") || !strings.Contains(body, `/dashboard/links/new?slug=zzzzz"`) {
		t.Error("Create CTA and its slug pre-fill must be unchanged when no candidate qualifies")
	}
}

// Scenario: HTMX Fragment Includes Suggestions — an unresolvable path
// requested with HX-Request renders the same did-you-mean block in the
// fragment.
func TestResolve404_DidYouMean_HTMXFragmentIncludesSuggestions(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLinkVis(t, "jira", env.userID, "public")

	w := env.resolveHTMX(t, "/jria", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("HX-Request must render a fragment, not the full page")
	}
	if !strings.Contains(body, "Did you mean") || !strings.Contains(body, `href="/jira"`) {
		t.Errorf("HTMX fragment must include the did-you-mean suggestions; body=%s", body)
	}
}

// REQ: at most 3 suggestions render, ordered by ascending distance with ties
// broken by slug ascending in byte order. doc/dock/docs are all distance 1
// from docz; doza qualifies at distance 2 but is cut by the cap.
func TestResolve404_DidYouMean_MaxThreeOrderedByDistanceThenSlug(t *testing.T) {
	env := newResolveTestEnv(t)
	for _, slug := range []string{"doza", "docs", "dock", "doc"} { // seeded out of order
		env.seedLinkVis(t, slug, env.userID, "public")
	}

	w := env.resolveAs(t, "/docz", nil)
	body := w.Body.String()
	if strings.Count(body, "Did you mean") != 1 {
		t.Fatalf("want exactly one did-you-mean block; body=%s", body)
	}
	iDoc := strings.Index(body, `href="/doc"`)
	iDock := strings.Index(body, `href="/dock"`)
	iDocs := strings.Index(body, `href="/docs"`)
	if iDoc == -1 || iDock == -1 || iDocs == -1 {
		t.Fatalf("all three distance-1 slugs must be suggested; body=%s", body)
	}
	if iDoc >= iDock || iDock >= iDocs {
		t.Error("equal-distance suggestions must be ordered by slug ascending (doc, dock, docs)")
	}
	if strings.Contains(body, `href="/doza"`) {
		t.Error("distance-2 candidate must be cut by the 3-suggestion cap")
	}
}

// REQ: alongside the visibility filter, expired and archived links are
// excluded from the candidate set for all callers (SPEC-0020).
func TestResolve404_DidYouMean_ExpiredAndArchivedExcluded(t *testing.T) {
	env := newResolveTestEnv(t)
	active := env.seedLinkVis(t, "jiraa", env.userID, "public")
	expired := env.seedLinkVis(t, "jirab", env.userID, "public")
	archived := env.seedLinkVis(t, "jirac", env.userID, "public")
	_ = active

	if _, err := env.db.Exec(env.db.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`),
		time.Now().UTC().Add(-time.Hour), expired.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	if _, err := env.db.Exec(env.db.Rebind(`UPDATE links SET archived_at = ? WHERE id = ?`),
		time.Now().UTC(), archived.ID); err != nil {
		t.Fatalf("archive link: %v", err)
	}

	w := env.resolveAs(t, "/jiraz", env.user())
	body := w.Body.String()
	if !strings.Contains(body, `href="/jiraa"`) {
		t.Errorf("active link must be suggested; body=%s", body)
	}
	if strings.Contains(body, `href="/jirab"`) || strings.Contains(body, `href="/jirac"`) {
		t.Error("expired and archived links must never be suggested — they would not resolve")
	}
}

// REQ oracle: an authenticated non-owner is no more privileged than an
// anonymous viewer for other users' private and secure slugs —
// discoverability, not resolvability, is the governing test.
func TestResolve404_DidYouMean_SecureAndPrivateNotLeakedToNonOwner(t *testing.T) {
	env := newResolveTestEnv(t)
	other := env.seedUser(t, "sub-other", "other@example.com", "Other")
	env.seedLinkVis(t, "secret", other.ID, "private")
	env.seedLinkVis(t, "secres", other.ID, "secure")

	for name, user := range map[string]*store.User{"anonymous": nil, "authenticated non-owner": env.user()} {
		t.Run(name, func(t *testing.T) {
			w := env.resolveAs(t, "/secrt", user)
			assertNoDidYouMean(t, w)
		})
	}
}

// REQ: admins MAY be offered any slug (SPEC-0010 REQ "Admin Visibility
// Override").
func TestResolve404_DidYouMean_AdminOfferedAnySlug(t *testing.T) {
	env := newResolveTestEnv(t)
	other := env.seedUser(t, "sub-other", "other@example.com", "Other")
	env.seedLinkVis(t, "secret", other.ID, "private")

	admin := &store.User{ID: env.userID, Role: "admin", DisplayName: "Admin", Email: "admin@example.com"}
	w := env.resolveAs(t, "/secrt", admin)
	if !strings.Contains(w.Body.String(), `href="/secret"`) {
		t.Error("admin should be offered another user's private slug")
	}
}

// REQ: did-you-mean is not computed for an empty or single-character
// requested path.
func TestResolve404_DidYouMean_NotComputedForEmptyOrSingleCharPath(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLinkVis(t, "za", env.userID, "public") // distance 1 from "z" — would qualify

	assertNoDidYouMean(t, env.resolveAs(t, "/z", env.user()))

	// The bare root path renders the 404 page with an empty slug; the helper
	// must decline it too (the route cannot exercise "" over HTTP).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := env.rh.didYouMeanSuggestions(req, ""); got != nil {
		t.Errorf("didYouMeanSuggestions(\"\") = %v, want nil", got)
	}
	if got := env.rh.didYouMeanSuggestions(req, "z"); got != nil {
		t.Errorf("didYouMeanSuggestions(\"z\") = %v, want nil", got)
	}
}

// REQ: only the first path segment of the requested path is matched — a
// multi-segment miss still suggests slugs near its first segment.
func TestResolve404_DidYouMean_FirstSegmentMatched(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLinkVis(t, "jira", env.userID, "public")

	w := env.resolveAs(t, "/jria/PROJ-123", env.user())
	body := w.Body.String()
	if !strings.Contains(body, "Did you mean") || !strings.Contains(body, `href="/jira"`) {
		t.Errorf("multi-segment miss should suggest slugs near the first segment; body=%s", body)
	}
}

// REQ: rendered slugs are HTML-escaped. A raw-seeded hostile slug (no
// creation surface allows one) must never reach the body unescaped, in text
// or href.
func TestResolve404_DidYouMean_RenderedSlugsHTMLEscaped(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedRawLink(t, `x<b>z`, "https://example.com")

	w := env.resolveAs(t, "/x%3Cb%3Ea", env.user())
	body := w.Body.String()
	if !strings.Contains(body, "Did you mean") {
		t.Fatalf("hostile slug at distance 1 should still be suggested (escaped); body=%s", body)
	}
	if strings.Contains(body, "x<b>z") {
		t.Error("suggested slug rendered unescaped in the 404 body")
	}
}

// A bare variable link's 404 (the slug exists, arity mismatched) must not
// suggest the very slug that just failed to resolve — that link loops back to
// the same 404 — while nearby slugs are still offered.
func TestResolve404_DidYouMean_RequestedSlugItselfNotSuggested(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "jira", "https://example.atlassian.net/browse/$ticket")
	env.seedLinkVis(t, "jirb", env.userID, "public")

	w := env.resolveAs(t, "/jira", env.user())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if strings.Contains(body, `href="/jira"`) {
		t.Error("the slug that just failed to resolve must not be suggested back")
	}
	if !strings.Contains(body, `href="/jirb"`) {
		t.Errorf("nearby slugs should still be suggested; body=%s", body)
	}
}
