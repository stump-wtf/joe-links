package handler

// 404 create-CTA gating tests (issue #260): the "Create this link" offer must
// only render for paths that could actually become links. Reserved slugs,
// format-invalid paths, and hostile input get no CTA at all — for signed-in
// and anonymous visitors alike.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// hostileSlug is a script-injection-shaped tag/slug that must never surface
// unescaped or in a create CTA.
const hostileSlug = `x');fetch('/evil')//`

// resolveAs issues a GET through the resolve route as the given user
// (nil = anonymous).
func (e *resolveTestEnv) resolveAs(t *testing.T, path string, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/{slug}*", e.rh.Resolve)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func (e *resolveTestEnv) user() *store.User {
	return &store.User{ID: e.userID, Role: "user", DisplayName: "Test", Email: "test@example.com"}
}

// assertNoCreateCTA asserts the 404 body offers no create path at all —
// neither the signed-in CTA nor the anonymous sign-in-to-create CTA.
func assertNoCreateCTA(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if strings.Contains(body, "Create this link") {
		t.Error("body offers \"Create this link\" for a non-creatable path")
	}
	if strings.Contains(body, "Sign in to create this link") {
		t.Error("body offers \"Sign in to create this link\" for a non-creatable path")
	}
	// Both CTA hrefs embed the prefill target; the bare nav "New link" button
	// (/dashboard/links/new without a query) is allowed.
	if strings.Contains(body, "/dashboard/links/new?slug=") || strings.Contains(body, "/dashboard/links/new%3Fslug%3D") {
		t.Error("body contains a prefilled create-link href for a non-creatable path")
	}
}

func TestCreatableCandidate(t *testing.T) {
	cases := []struct {
		path          string
		wantCandidate string
		wantCreatable bool
	}{
		{"", "", false},
		{"my-new-link", "my-new-link", true},
		{"a", "a", true},
		{"team/roadmap/q3", "team", true},
		{"team/", "team", true},
		{"admin", "", false},               // reserved
		{"u/ghost-user", "", false},        // reserved first segment
		{"UPPER", "", false},               // uppercase
		{"snake_case", "", false},          // underscore
		{"-leading-dash", "", false},       // bad first char
		{"trailing-dash-", "", false},      // bad last char
		{"dot.ted", "", false},             // punctuation
		{"BAD/good-segment", "", false},    // first segment decides
		{hostileSlug, "", false},           // hostile charset
		{"javascript:alert(1)", "", false}, // scheme-shaped input
	}
	for _, tc := range cases {
		candidate, creatable := creatableCandidate(tc.path)
		if candidate != tc.wantCandidate || creatable != tc.wantCreatable {
			t.Errorf("creatableCandidate(%q) = (%q, %v), want (%q, %v)",
				tc.path, candidate, creatable, tc.wantCandidate, tc.wantCreatable)
		}
	}
	// Every reserved slug must be refused, derived from the canonical set so
	// this test can never drift from internal/store/validate.go.
	for _, slug := range store.ReservedSlugs() {
		if _, creatable := creatableCandidate(slug); creatable {
			t.Errorf("creatableCandidate(%q) = creatable, want refused (reserved)", slug)
		}
	}
}

func TestResolve404_CTA_ValidSlug_SignedIn(t *testing.T) {
	env := newResolveTestEnv(t)

	w := env.resolveAs(t, "/my-new-link", env.user())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Create this link") {
		t.Error("signed-in 404 for a creatable slug should offer \"Create this link\"")
	}
	if !strings.Contains(body, `/dashboard/links/new?slug=my-new-link"`) {
		t.Errorf("CTA should pre-fill the creatable slug; body=%s", body)
	}
}

func TestResolve404_CTA_MultiSegment_PrefillsFirstSegment(t *testing.T) {
	env := newResolveTestEnv(t)

	w := env.resolveAs(t, "/team/roadmap/q3", env.user())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if !strings.Contains(body, `/dashboard/links/new?slug=team"`) {
		t.Errorf("multi-segment CTA should pre-fill the first segment; body=%s", body)
	}
	if strings.Contains(body, "?slug=team/") || strings.Contains(body, "?slug=team%2f") || strings.Contains(body, "?slug=team%2F") {
		t.Error("CTA must not pre-fill the full multi-segment path — it can never validate")
	}
}

func TestResolve404_NoCTA_ReservedPrefix_SignedIn(t *testing.T) {
	env := newResolveTestEnv(t)

	for _, path := range []string{"/mcp", "/u/ghost-user", "/api/v9/nope"} {
		t.Run(path, func(t *testing.T) {
			assertNoCreateCTA(t, env.resolveAs(t, path, env.user()))
		})
	}
}

func TestResolve404_NoCTA_InvalidCharset_SignedIn(t *testing.T) {
	env := newResolveTestEnv(t)

	for _, path := range []string{"/UPPER", "/snake_case", "/-leading-dash", "/trailing-dash-", "/dot.ted"} {
		t.Run(path, func(t *testing.T) {
			assertNoCreateCTA(t, env.resolveAs(t, path, env.user()))
		})
	}
}

func TestResolve404_Anonymous_CreatableSlug_SignInCTA(t *testing.T) {
	env := newResolveTestEnv(t)

	w := env.resolveAs(t, "/my-new-link", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Sign in to create this link") {
		t.Error("anonymous 404 for a creatable slug should keep the sign-in CTA")
	}
	if !strings.Contains(body, "/dashboard/links/new%3Fslug%3Dmy-new-link") {
		t.Errorf("sign-in CTA should carry the prefilled return_url; body=%s", body)
	}
}

func TestResolve404_Anonymous_NonCreatable_NoCTAAtAll(t *testing.T) {
	env := newResolveTestEnv(t)

	for _, path := range []string{"/admin", "/UPPER", "/" + url.PathEscape(hostileSlug)} {
		t.Run(path, func(t *testing.T) {
			assertNoCreateCTA(t, env.resolveAs(t, path, nil))
		})
	}
}

// A variable link visited with no variable segments 404s, but its slug is
// taken — offering to create it would be a guaranteed ErrSlugTaken dead end.
func TestResolve404_NoCTA_ExistingVariableLink_NoArgs(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "jira", "https://example.atlassian.net/browse/$ticket")

	for name, user := range map[string]*store.User{"signed-in": env.user(), "anonymous": nil} {
		t.Run(name, func(t *testing.T) {
			assertNoCreateCTA(t, env.resolveAs(t, "/jira", user))
		})
	}
}

// An arity mismatch on an existing variable link 404s, but the matched prefix
// already exists and longer prefixes win resolution — no create offer.
func TestResolve404_NoCTA_ExistingVariableLink_ArityMismatch(t *testing.T) {
	env := newResolveTestEnv(t)
	env.seedLink(t, "jira", "https://example.atlassian.net/browse/$ticket")

	for _, path := range []string{"/jira/ABC-1/extra", "/jira/a/b/c"} {
		t.Run(path, func(t *testing.T) {
			assertNoCreateCTA(t, env.resolveAs(t, path, env.user()))
		})
	}
}

// A script-injection-shaped path must get no CTA, and the echoed slug must be
// HTML-escaped everywhere it appears (issue #212's class of bug).
func TestResolve404_HostileSlug_NoCTAAndEscaped(t *testing.T) {
	env := newResolveTestEnv(t)

	w := env.resolveAs(t, "/"+url.PathEscape(hostileSlug), env.user())
	assertNoCreateCTA(t, w)
	if strings.Contains(w.Body.String(), hostileSlug) {
		t.Error("hostile slug rendered unescaped in the 404 body")
	}
}

// A javascript: scheme-shaped path must get no CTA and must never appear as a
// link target.
func TestResolve404_JavascriptScheme_NoCTA(t *testing.T) {
	env := newResolveTestEnv(t)

	w := env.resolveAs(t, "/javascript:alert(1)", env.user())
	assertNoCreateCTA(t, w)
	body := w.Body.String()
	if strings.Contains(body, `href="javascript:`) || strings.Contains(body, `href='javascript:`) {
		t.Error("404 body contains a javascript: href")
	}
}
