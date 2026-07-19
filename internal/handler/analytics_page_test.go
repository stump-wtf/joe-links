package handler

// Story #278 — global analytics dashboard page (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "No cross-user leakage"
//   - "Admin toggle" (web half: the visible toggle is admin-only and
//     scope=all from a non-admin renders the forbidden page; the API half is
//     pinned in internal/api/analytics_test.go)
//
// plus the retention relabeling of the never-clicked panel (REQ "Click
// Retention": "no clicks within retention") and the HTMX fragment behavior
// of the period toggle.
//
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces", ADR-0021

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

type analyticsPageEnv struct {
	router http.Handler
	db     *sqlx.DB
	links  *store.LinkStore
	users  *store.UserStore
	clicks *store.ClickStore
}

// newAnalyticsPageEnv mirrors NewRouter's analytics wiring (retention off).
func newAnalyticsPageEnv(t *testing.T, retentionDays int) *analyticsPageEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tagStore := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tagStore)
	us := store.NewUserStore(db)
	cs := store.NewClickStore(db)

	h := NewAnalyticsHandler(cs, retentionDays)
	r := chi.NewRouter()
	r.Get("/dashboard/analytics", h.Show)
	return &analyticsPageEnv{router: r, db: db, links: ls, users: us, clicks: cs}
}

// get issues a GET as the given user; htmx toggles the HX-Request header.
func (e *analyticsPageEnv) get(t *testing.T, path string, user *store.User, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// seedUserNamed creates a user with a distinct display name so leak
// assertions cannot false-positive on a shared name.
func (e *analyticsPageEnv) seedUserNamed(t *testing.T, key, name, role string) *store.User {
	t.Helper()
	u, err := e.users.Upsert(context.Background(), "test", key, key+"@example.com", name, role)
	if err != nil {
		t.Fatalf("seed user %s: %v", key, err)
	}
	if role != "" && u.Role != role {
		u, err = e.users.UpdateRole(context.Background(), u.ID, role)
		if err != nil {
			t.Fatalf("set role: %v", err)
		}
	}
	return u
}

// Scenario: No cross-user leakage — user A opens /dashboard/analytics while
// user B owns a heavily-clicked public link A neither co-owns nor has a
// share for; B's link appears in none of A's panels and contributes to none
// of A's counts.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", Security
// Requirements "Cross-User Aggregation Leakage"
func TestGlobalAnalytics_NoCrossUserLeakage(t *testing.T) {
	env := newAnalyticsPageEnv(t, 0)
	ctx := context.Background()
	userA := env.seedUserNamed(t, "ga-user-a", "User Alpha", "user")
	userB := env.seedUserNamed(t, "ga-user-b", "User Bravo", "user")

	// A's own clicked link, so A's page has real content of A's own.
	mine, err := env.links.Create(ctx, "ga-mine", "https://example.com/mine", userA.ID, "Mine", "", "private")
	if err != nil {
		t.Fatalf("create A's link: %v", err)
	}
	// B's heavily-clicked *public* link — the visibility most tempting to
	// include, and exactly the one that must not leak.
	theirs, err := env.links.Create(ctx, "ga-theirs", "https://example.com/theirs", userB.ID, "Theirs", "", "public")
	if err != nil {
		t.Fatalf("create B's link: %v", err)
	}
	base := utcMidnightToday().AddDate(0, 0, -1).Add(12 * time.Hour)
	seedClickAt(t, env.db, mine.ID, base, "")
	for i := 0; i < 50; i++ {
		seedClickAt(t, env.db, theirs.ID, base.Add(time.Duration(i)*time.Second), "")
	}

	w := env.get(t, "/dashboard/analytics", userA, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "ga-mine") {
		t.Errorf("A's own link must appear in A's panels")
	}
	if strings.Contains(body, "ga-theirs") || strings.Contains(body, theirs.ID) {
		t.Errorf("B's public link must appear in none of A's panels; body=%s", body)
	}
	// B's 50 clicks must contribute to none of A's counts.
	if strings.Contains(body, ">50<") || strings.Contains(body, ">51<") {
		t.Errorf("B's click volume must not contribute to A's counts; body=%s", body)
	}
}

// Scenario: Admin toggle (web) — the scope toggle is visible only to admins;
// an admin requesting scope=all gets instance-wide panels while a non-admin
// requesting scope=all gets the forbidden page; without the parameter both
// get their personal scope.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces"
func TestGlobalAnalytics_AdminToggle(t *testing.T) {
	env := newAnalyticsPageEnv(t, 0)
	ctx := context.Background()
	admin := env.seedUserNamed(t, "ga-admin", "Admin Ada", "admin")
	member := env.seedUserNamed(t, "ga-member", "Member Mel", "user")

	link, err := env.links.Create(ctx, "ga-member-link", "https://example.com/m", member.ID, "M", "", "public")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	seedClickAt(t, env.db, link.ID, utcMidnightToday().AddDate(0, 0, -1).Add(12*time.Hour), "")

	// The admin's default view is the personal scope — no member link — but
	// carries the visible scope toggle.
	adminDefault := env.get(t, "/dashboard/analytics", admin, false)
	if adminDefault.Code != http.StatusOK {
		t.Fatalf("admin default status = %d, want 200", adminDefault.Code)
	}
	adminBody := adminDefault.Body.String()
	if !strings.Contains(adminBody, "scope=all") {
		t.Errorf("admin page must render the explicit scope=all toggle")
	}
	if strings.Contains(adminBody, "ga-member-link") {
		t.Errorf("admin default scope must be personal, not instance-wide; body=%s", adminBody)
	}

	// scope=all: instance-wide aggregates for the admin.
	adminAll := env.get(t, "/dashboard/analytics?scope=all", admin, false)
	if adminAll.Code != http.StatusOK {
		t.Fatalf("admin scope=all status = %d, want 200", adminAll.Code)
	}
	if !strings.Contains(adminAll.Body.String(), "ga-member-link") {
		t.Errorf("admin scope=all must include the member's link; body=%s", adminAll.Body.String())
	}

	// Non-admins get no toggle, and scope=all is refused with the forbidden page.
	memberDefault := env.get(t, "/dashboard/analytics", member, false)
	if memberDefault.Code != http.StatusOK {
		t.Fatalf("member default status = %d, want 200", memberDefault.Code)
	}
	if strings.Contains(memberDefault.Body.String(), "scope=all") {
		t.Errorf("non-admin page must not render the scope=all toggle")
	}
	memberAll := env.get(t, "/dashboard/analytics?scope=all", member, false)
	if memberAll.Code != http.StatusForbidden {
		t.Errorf("non-admin scope=all status = %d, want 403", memberAll.Code)
	}
}

// With retention enabled, the never-clicked panel is relabeled "no clicks
// within retention" — a link whose entire history was pruned is
// indistinguishable from an unclicked one, and the label says so.
// Governing: SPEC-0021 REQ "Click Retention", REQ "Global Analytics Dashboard"
func TestGlobalAnalytics_NeverClickedLabeledUnderRetention(t *testing.T) {
	// Both envs share the test's in-memory database; only the handlers'
	// retention configuration differs.
	off := newAnalyticsPageEnv(t, 0)
	on := newAnalyticsPageEnv(t, 365)
	user := off.seedUserNamed(t, "ga-ret", "Ret Viewer", "user")

	link, err := off.links.Create(context.Background(), "ga-aged", "https://example.com/aged", user.ID, "Aged", "", "private")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	// Age the link past the 7-day creation grace so it qualifies.
	if _, err := off.db.Exec(off.db.Rebind(`UPDATE links SET created_at = ? WHERE id = ?`),
		time.Now().UTC().AddDate(0, 0, -30), link.ID); err != nil {
		t.Fatalf("age link: %v", err)
	}

	// Retention off: the plain label.
	offBody := off.get(t, "/dashboard/analytics", user, false).Body.String()
	if !strings.Contains(offBody, "Never clicked") || !strings.Contains(offBody, "ga-aged") {
		t.Errorf("retention-off panel must list the aged link under the plain label; body=%s", offBody)
	}

	// Retention on: the honest label.
	onBody := on.get(t, "/dashboard/analytics", user, false).Body.String()
	if !strings.Contains(onBody, "No clicks within retention") {
		t.Errorf("retention-on panel label must read 'no clicks within retention'; body=%s", onBody)
	}
	if strings.Contains(onBody, "Never clicked") {
		t.Errorf("retention-on panel must not keep the plain never-clicked label")
	}
}

// The period toggle is an HTMX fragment swap: an HX-Request for
// period=month returns the content fragment (not a full page) covering the
// 30-day window, with the toggle wired to the analytics root.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard" — HTMX fragment
// behavior per house pattern
func TestGlobalAnalytics_PeriodToggleSwapsFragment(t *testing.T) {
	env := newAnalyticsPageEnv(t, 0)
	user := env.seedUserNamed(t, "ga-period", "Period P", "user")

	page := env.get(t, "/dashboard/analytics", user, false)
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d, want 200", page.Code)
	}
	pageBody := page.Body.String()
	if !strings.Contains(pageBody, `hx-get="/dashboard/analytics?period=month"`) {
		t.Errorf("page must wire the month toggle via HTMX; body=%s", pageBody)
	}
	if !strings.Contains(pageBody, `hx-target="#analytics-root"`) {
		t.Errorf("toggles must target the analytics root container")
	}

	frag := env.get(t, "/dashboard/analytics?period=month", user, true)
	if frag.Code != http.StatusOK {
		t.Fatalf("fragment status = %d, want 200; body=%s", frag.Code, frag.Body.String())
	}
	fragBody := frag.Body.String()
	if strings.Contains(fragBody, "<html") {
		t.Errorf("HTMX response must be a fragment, not a full page")
	}
	if !strings.Contains(fragBody, `id="analytics-root"`) || !strings.Contains(fragBody, "last 30 days") {
		t.Errorf("fragment must cover the 30-day month window; body=%s", fragBody)
	}
}
