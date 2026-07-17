package handler

// Regression tests for issue #196: HEAD requests to short links returned 405.
// These deliberately exercise the real NewRouter (not a private test router) so
// that removing middleware.GetHead from the middleware stack fails them.
// Governing: SPEC-0001 REQ "Short Link Resolution"

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newRouterTestEnv builds the full production router via NewRouter on an
// in-memory database, seeded with one public link. The click channel is
// buffered so redirect's non-blocking send always lands when recording is
// expected.
func newRouterTestEnv(t *testing.T) (http.Handler, chan store.ClickEvent) {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)

	u, err := us.Upsert(context.Background(), "test", "sub1", "head@example.com", "Head Tester", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := ls.Create(context.Background(), "example", "https://example.com/target", u.ID, "", "", "public"); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if _, err := ls.Create(context.Background(), "gh", "https://github.com/$user", u.ID, "", "", "public"); err != nil {
		t.Fatalf("seed variable link: %v", err)
	}

	sm := scs.New()
	clickCh := make(chan store.ClickEvent, 8)
	router := NewRouter(Deps{
		SessionManager: sm,
		AuthHandlers:   auth.NewHandlers(nil, sm, us, "", nil, "groups", true),
		AuthMiddleware: auth.NewMiddleware(sm, us),
		LinkStore:      ls,
		OwnershipStore: owns,
		TagStore:       tags,
		UserStore:      us,
		TokenStore:     auth.NewSQLTokenStore(db),
		KeywordStore:   store.NewKeywordStore(db),
		ClickStore:     store.NewClickStore(db),
		ClickCh:        clickCh,
	})
	return router, clickCh
}

func TestRouter_HeadResolvesLikeGet(t *testing.T) {
	router, clickCh := newRouterTestEnv(t)

	req := httptest.NewRequest(http.MethodHead, "/example", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("HEAD /example status = %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "https://example.com/target" {
		t.Errorf("Location = %q, want %q", loc, "https://example.com/target")
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD response body length = %d, want 0", w.Body.Len())
	}
	// HEAD probes must not record clicks (bots would inflate stats).
	// Governing: SPEC-0016 REQ "Click Recording", ADR-0016
	if n := len(clickCh); n != 0 {
		t.Errorf("click events recorded for HEAD = %d, want 0", n)
	}
}

// TestRouter_GetStillRecordsClicks is the control for the HEAD no-click
// assertion above: the same router wiring does record clicks for GET, proving
// the empty channel after HEAD is the method check and not dead wiring.
func TestRouter_GetStillRecordsClicks(t *testing.T) {
	router, clickCh := newRouterTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/example", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("GET /example status = %d, want %d", w.Code, http.StatusFound)
	}
	if n := len(clickCh); n != 1 {
		t.Errorf("click events recorded for GET = %d, want 1", n)
	}
}

// GetHead is router-wide, not resolver-specific: HEAD on other GET routes
// (here the public link browser, and by the same mechanism the mounted
// /api/v1 sub-router) must answer instead of 405ing.
func TestRouter_HeadNonResolverRoute(t *testing.T) {
	router, _ := newRouterTestEnv(t)

	req := httptest.NewRequest(http.MethodHead, "/links", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD /links status = %d, want %d", w.Code, http.StatusOK)
	}

	// The mounted /api/v1 sub-router gets the same treatment: chi's GetHead
	// lookahead recurses through Mount. /api/v1/config is the public,
	// unauthenticated endpoint (SPEC-0005), so 200 proves the rewrite.
	req = httptest.NewRequest(http.MethodHead, "/api/v1/config", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD /api/v1/config status = %d, want %d", w.Code, http.StatusOK)
	}
}

// HEAD on an arity-matched variable link substitutes and redirects like GET,
// and — like the static-link case — records no click.
func TestRouter_HeadVariableLink(t *testing.T) {
	router, clickCh := newRouterTestEnv(t)

	req := httptest.NewRequest(http.MethodHead, "/gh/joestump", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("HEAD /gh/joestump status = %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "https://github.com/joestump" {
		t.Errorf("Location = %q, want %q", loc, "https://github.com/joestump")
	}
	if n := len(clickCh); n != 0 {
		t.Errorf("click events recorded for HEAD variable link = %d, want 0", n)
	}
}

// HEAD on a 404 slug behaves like GET's 404, not 405.
func TestRouter_HeadUnknownSlug404(t *testing.T) {
	router, _ := newRouterTestEnv(t)

	req := httptest.NewRequest(http.MethodHead, "/no-such-slug", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("HEAD /no-such-slug status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// TestReservedSlugs_CoverEveryTopLevelRoute derives the route table from the
// REAL router (chi.Walk over NewRouter) instead of a hand-maintained list, so
// adding a top-level route without reserving its first segment fails here —
// the drift mechanism issue #204 killed cannot re-enter via this test.
// Governing: SPEC-0002 — reserved slugs shadow no registered route.
func TestReservedSlugs_CoverEveryTopLevelRoute(t *testing.T) {
	router, _ := newRouterTestEnv(t)

	chiRouter, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter no longer returns a chi.Routes; rework this test")
	}

	routeSegments := map[string]bool{}
	err := chi.Walk(chiRouter, func(method, route string, h http.Handler, mw ...func(http.Handler) http.Handler) error {
		seg := strings.TrimPrefix(route, "/")
		if i := strings.IndexByte(seg, '/'); i >= 0 {
			seg = seg[:i]
		}
		// Skip the root route and the parameterized catch-all — only static
		// top-level segments need reservation.
		if seg == "" || seg == "*" || strings.HasPrefix(seg, "{") {
			return nil
		}
		routeSegments[seg] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}

	reserved := map[string]bool{}
	for _, s := range store.ReservedSlugs() {
		reserved[s] = true
	}

	for seg := range routeSegments {
		if !reserved[seg] {
			t.Errorf("top-level route segment %q is not in store.ReservedSlugs() — a user could own the slug and shadow-confuse the route", seg)
		}
	}
	for slug := range reserved {
		if !routeSegments[slug] {
			t.Errorf("reserved slug %q matches no registered top-level route — stale reservation", slug)
		}
	}
}
