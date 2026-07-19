// Web tests for the SPEC-0020 REQ "Health Badges and Admin Report" and REQ
// "Staleness Views" scenarios: the capability-gated "broken" badge on
// dashboard rows, the health-free public browser, the admin report at
// /admin/link-health, and the dashboard staleness filters. Tests are named
// after the spec scenarios so the spec↔test mapping is auditable.
//
// Governing: SPEC-0020 REQ "Health Badges and Admin Report", REQ "Staleness Views", ADR-0020
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// brokenBadgeMarkup is the badge text the shared link_row partial renders;
// asserting on the closing tag avoids false positives from unrelated copy.
const brokenBadgeMarkup = ">broken</span>"

type healthBadgeEnv struct {
	db    *sqlx.DB
	links *store.LinkStore
	users *store.UserStore
	owns  *store.OwnershipStore
	tags  *store.TagStore
	ks    *store.KeywordStore
	owner *store.User
	admin *store.User
}

func newHealthBadgeEnv(t *testing.T) *healthBadgeEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	ks := store.NewKeywordStore(db)

	ctx := context.Background()
	owner, err := us.Upsert(ctx, "test", "hb-owner", "hb-owner@example.com", "Badge Owner", "user")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "hb-admin", "hb-admin@example.com", "Badge Admin", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return &healthBadgeEnv{db: db, links: ls, users: us, owns: owns, tags: tags, ks: ks, owner: owner, admin: admin}
}

func (e *healthBadgeEnv) createLink(t *testing.T, slug, visibility string, ownerID string) *store.Link {
	t.Helper()
	link, err := e.links.CreateFull(context.Background(), slug, "https://example.com/"+slug, ownerID, "", "", visibility, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("create link %s: %v", slug, err)
	}
	return link
}

// breakLink records enough consecutive failures to derive "broken".
func (e *healthBadgeEnv) breakLink(t *testing.T, linkID string) {
	t.Helper()
	status := 503
	for i := 0; i < store.HealthBrokenThreshold; i++ {
		if _, err := e.links.RecordHealthFailure(context.Background(), linkID, &status, "unavailable", time.Now().UTC(), time.Hour); err != nil {
			t.Fatalf("seed failure: %v", err)
		}
	}
}

func (e *healthBadgeEnv) get(t *testing.T, h http.HandlerFunc, path string, user *store.User, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// Scenario: Broken Badge on Owner Dashboard
// WHEN an owner's link has 3 or more consecutive failed checks THEN the
// owner's dashboard row for that link shows a "broken" badge.
func TestHealthBadges_BrokenBadgeOnOwnerDashboard(t *testing.T) {
	env := newHealthBadgeEnv(t)
	broken := env.createLink(t, "rotting", "public", env.owner.ID)
	env.breakLink(t, broken.ID)
	env.createLink(t, "fine", "public", env.owner.ID)

	dashboard := NewDashboardHandler(env.links, env.owns, env.tags, env.ks)
	rec := env.get(t, dashboard.Show, "/dashboard", env.owner, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, brokenBadgeMarkup) {
		t.Errorf("dashboard row missing the broken badge; body=%s", body)
	}
	if got := strings.Count(body, brokenBadgeMarkup); got != 1 {
		t.Errorf("broken badge rendered %d times, want exactly 1 (the healthy link must not carry it)", got)
	}
}

// The badge disappears the moment the owner opts the link out: the frozen
// health row is no longer surfaced (REQ "Destination Health Checking"
// scenario "Opt-Out Honored", badge half).
func TestHealthBadges_OptOutHidesBrokenBadge(t *testing.T) {
	env := newHealthBadgeEnv(t)
	broken := env.createLink(t, "rotting", "public", env.owner.ID)
	env.breakLink(t, broken.ID)
	if _, err := env.links.SetHealthChecksDisabled(context.Background(), broken.ID, true); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	dashboard := NewDashboardHandler(env.links, env.owns, env.tags, env.ks)
	rec := env.get(t, dashboard.Show, "/dashboard", env.owner, true)
	if strings.Contains(rec.Body.String(), brokenBadgeMarkup) {
		t.Error("broken badge rendered for an opted-out link; the frozen health row must not be surfaced")
	}
}

// A broken badge never renders on an archived or expired row — its frozen
// health row is not surfaced, so only the lifecycle badge appears.
func TestHealthBadges_LifecycleBadgeSuppressesBrokenBadge(t *testing.T) {
	env := newHealthBadgeEnv(t)
	archived := env.createLink(t, "was-archived", "public", env.owner.ID)
	env.breakLink(t, archived.ID)
	if _, err := env.links.SetArchived(context.Background(), archived.ID, true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	expired := env.createLink(t, "was-expired", "public", env.owner.ID)
	env.breakLink(t, expired.ID)
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := env.db.Exec(env.db.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`), past, expired.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	dashboard := NewDashboardHandler(env.links, env.owns, env.tags, env.ks)
	rec := env.get(t, dashboard.Show, "/dashboard", env.owner, true)
	body := rec.Body.String()
	if strings.Contains(body, brokenBadgeMarkup) {
		t.Errorf("broken badge rendered on an archived/expired row; body=%s", body)
	}
	if !strings.Contains(body, ">archived</span>") || !strings.Contains(body, ">expired</span>") {
		t.Errorf("lifecycle badges missing; body=%s", body)
	}
}

// Scenario: Public Browser Shows No Health Data
// WHEN an anonymous visitor browses public links THEN no health badges or
// check data are rendered, and expired/archived links do not appear at all.
func TestHealthBadges_PublicBrowserShowsNoHealthData(t *testing.T) {
	env := newHealthBadgeEnv(t)
	live := env.createLink(t, "public-live", "public", env.owner.ID)
	env.breakLink(t, live.ID) // broken but public: badge must not render here
	expired := env.createLink(t, "public-expired", "public", env.owner.ID)
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := env.db.Exec(env.db.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`), past, expired.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	archived := env.createLink(t, "public-archived", "public", env.owner.ID)
	if _, err := env.links.SetArchived(context.Background(), archived.ID, true); err != nil {
		t.Fatalf("archive: %v", err)
	}

	public := NewPublicLinksHandler(env.links, env.ks)
	rec := env.get(t, public.Index, "/links", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("public browser status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "public-live") {
		t.Errorf("live public link missing from the browser; body=%s", body)
	}
	if strings.Contains(body, brokenBadgeMarkup) {
		t.Error("health badge rendered on the public browser")
	}
	if strings.Contains(body, "public-expired") || strings.Contains(body, "public-archived") {
		t.Error("expired/archived public links are enumerable on the public browser")
	}
}

// Scenario: Admin Report Lists Failing Links
// WHEN an admin visits /admin/link-health while two links are broken THEN
// both are listed with status, owner, and last-checked details; healthy,
// opted-out, skipped, and never-checked links are not listed.
func TestHealthBadges_AdminReportListsFailingLinks(t *testing.T) {
	env := newHealthBadgeEnv(t)
	ctx := context.Background()

	worse := env.createLink(t, "worse", "public", env.owner.ID)
	env.breakLink(t, worse.ID)
	status := 503
	if _, err := env.links.RecordHealthFailure(ctx, worse.ID, &status, "unavailable", time.Now().UTC(), time.Hour); err != nil {
		t.Fatalf("extra failure: %v", err)
	}
	broken := env.createLink(t, "merely-broken", "public", env.owner.ID)
	env.breakLink(t, broken.ID)

	healthy := env.createLink(t, "healthy", "public", env.owner.ID)
	if _, err := env.links.RecordHealthSuccess(ctx, healthy.ID, 200, time.Now().UTC(), time.Hour); err != nil {
		t.Fatalf("seed healthy: %v", err)
	}
	optedOut := env.createLink(t, "opted-out", "public", env.owner.ID)
	env.breakLink(t, optedOut.ID)
	if _, err := env.links.SetHealthChecksDisabled(ctx, optedOut.ID, true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	skipped := env.createLink(t, "skipped", "public", env.owner.ID)
	if _, err := env.links.RecordHealthSkipped(ctx, skipped.ID, "policy", time.Now().UTC(), time.Hour); err != nil {
		t.Fatalf("seed skipped: %v", err)
	}
	env.createLink(t, "never-checked", "public", env.owner.ID)

	adminH := NewAdminHandler(env.links, env.users, env.ks)
	rec := env.get(t, adminH.LinkHealth, "/admin/link-health", env.admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("report status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"worse", "merely-broken", "503", "Badge Owner", "/dashboard/links/" + worse.ID} {
		if !strings.Contains(body, want) {
			t.Errorf("report missing %q; body=%s", want, body)
		}
	}
	for _, absent := range []string{"healthy", "opted-out", ">skipped<", "never-checked"} {
		if strings.Contains(body, absent) {
			t.Errorf("report lists %q, which must not appear", absent)
		}
	}
	// Most failures first: "worse" (4) sorts before "merely-broken" (3).
	if strings.Index(body, "worse") > strings.Index(body, "merely-broken") {
		t.Error("report rows not ordered by most failures first")
	}
}

// Scenario: Stale Filter (dashboard wiring)
// WHEN an owner applies the "stale" filter THEN the dashboard renders only
// links created > 90 days ago with no click in the last 90 days.
func TestStaleness_StaleFilterOnDashboard(t *testing.T) {
	env := newHealthBadgeEnv(t)
	now := time.Now().UTC()

	stale := env.createLink(t, "stale-old", "public", env.owner.ID)
	backdate(t, env.db, stale.ID, now.Add(-180*24*time.Hour))
	plantClick(t, env.db, stale.ID, now.Add(-120*24*time.Hour))

	fresh := env.createLink(t, "clicked-yesterday", "public", env.owner.ID)
	backdate(t, env.db, fresh.ID, now.Add(-180*24*time.Hour))
	plantClick(t, env.db, fresh.ID, now.Add(-24*time.Hour))

	dashboard := NewDashboardHandler(env.links, env.owns, env.tags, env.ks)
	rec := env.get(t, dashboard.Show, "/dashboard?filter=stale", env.owner, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "stale-old") {
		t.Errorf("stale filter missing the stale link; body=%s", body)
	}
	if strings.Contains(body, "clicked-yesterday") {
		t.Error("stale filter includes a link clicked yesterday")
	}
}

// Scenario: Never-Clicked Filter (dashboard wiring)
// WHEN an owner applies the "never clicked" filter THEN a 3-week-old
// unclicked link appears and a 2-day-old one does not.
func TestStaleness_NeverClickedFilterOnDashboard(t *testing.T) {
	env := newHealthBadgeEnv(t)
	now := time.Now().UTC()

	old := env.createLink(t, "unclicked-old", "public", env.owner.ID)
	backdate(t, env.db, old.ID, now.Add(-21*24*time.Hour))
	env.createLink(t, "unclicked-new", "public", env.owner.ID)

	dashboard := NewDashboardHandler(env.links, env.owns, env.tags, env.ks)
	rec := env.get(t, dashboard.Show, "/dashboard?filter=never-clicked", env.owner, true)
	body := rec.Body.String()
	if !strings.Contains(body, "unclicked-old") {
		t.Errorf("never-clicked filter missing the 3-week-old link; body=%s", body)
	}
	if strings.Contains(body, "unclicked-new") {
		t.Error("never-clicked filter includes a link inside the 7-day grace period")
	}
}

// Scenario: Staleness Respects Visibility Scope
// WHEN a non-admin user applies a staleness filter THEN the results contain
// only links already visible to them on the dashboard under SPEC-0010.
func TestStaleness_StalenessRespectsVisibilityScopeOnDashboard(t *testing.T) {
	env := newHealthBadgeEnv(t)
	now := time.Now().UTC()

	mine := env.createLink(t, "my-stale", "public", env.owner.ID)
	backdate(t, env.db, mine.ID, now.Add(-100*24*time.Hour))
	theirs := env.createLink(t, "their-stale", "public", env.admin.ID)
	backdate(t, env.db, theirs.ID, now.Add(-100*24*time.Hour))

	dashboard := NewDashboardHandler(env.links, env.owns, env.tags, env.ks)
	rec := env.get(t, dashboard.Show, "/dashboard?filter=stale", env.owner, true)
	body := rec.Body.String()
	if !strings.Contains(body, "my-stale") {
		t.Errorf("owner's own stale link missing; body=%s", body)
	}
	if strings.Contains(body, "their-stale") {
		t.Error("staleness filter leaked another user's link to a non-admin")
	}

	// Admins see the full scope — the same filter across all links.
	recAdmin := env.get(t, dashboard.Show, "/dashboard?filter=stale", env.admin, true)
	adminBody := recAdmin.Body.String()
	if !strings.Contains(adminBody, "my-stale") || !strings.Contains(adminBody, "their-stale") {
		t.Errorf("admin staleness scope missing links; body=%s", adminBody)
	}
}

// The web edit form is the third surface accepting the opt-out (REQ
// "Destination Health Checking" — Eligibility: "editable with CanEdit via the
// link form and API"): checking the box sets the flag, and resubmitting the
// form without it clears the flag (checkbox semantics).
func TestHealthBadges_EditFormTogglesOptOut(t *testing.T) {
	env := newHealthBadgeEnv(t)
	link := env.createLink(t, "form-toggle", "public", env.owner.ID)

	us := env.users
	links := NewLinksHandler(env.links, env.owns, us, env.ks)
	r := chi.NewRouter()
	r.Put("/dashboard/links/{id}", links.Update)

	putForm := func(t *testing.T, form url.Values) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/dashboard/links/"+link.ID, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, env.owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("update status = %d, want 200: %s", w.Code, w.Body.String())
		}
	}

	putForm(t, url.Values{"url": {"https://example.com/form-toggle"}, "health_checks_disabled": {"1"}})
	after, err := env.links.GetByID(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("reload link: %v", err)
	}
	if !after.HealthChecksDisabled {
		t.Error("checkbox checked but health_checks_disabled = false")
	}

	// Checkbox absent (unchecked) on the always-submitting edit form: cleared.
	putForm(t, url.Values{"url": {"https://example.com/form-toggle"}})
	after, err = env.links.GetByID(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("reload link: %v", err)
	}
	if after.HealthChecksDisabled {
		t.Error("checkbox absent but health_checks_disabled stayed true")
	}
}

func backdate(t *testing.T, db *sqlx.DB, linkID string, createdAt time.Time) {
	t.Helper()
	if _, err := db.Exec(db.Rebind(`UPDATE links SET created_at = ? WHERE id = ?`), createdAt.UTC(), linkID); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}
}

func plantClick(t *testing.T, db *sqlx.DB, linkID string, clickedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(db.Rebind(`
		INSERT INTO link_clicks (id, link_id, user_id, ip_hash, user_agent, referrer, clicked_at)
		VALUES (?, ?, NULL, 'test-hash', '', '', ?)
	`), uuid.NewString(), linkID, clickedAt.UTC()); err != nil {
		t.Fatalf("insert click: %v", err)
	}
}
