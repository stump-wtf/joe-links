// Web tests for the SPEC-0020 REQ "Archive State" and REQ "Renewal"
// scenarios: the archive/unarchive toggle endpoints, the one-click renew
// action, their CanEdit capability gates, and the dashboard row rendering the
// renew action next to the expired badge. Tests are named after the spec
// scenarios so the spec↔test mapping is auditable.
//
// Governing: SPEC-0020 REQ "Archive State", REQ "Renewal", ADR-0020
package handler

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

type archiveRenewEnv struct {
	router    http.Handler
	db        *sqlx.DB
	links     *store.LinkStore
	clicks    *store.ClickStore
	owner     *store.User
	recipient *store.User
	stranger  *store.User
}

// newArchiveRenewEnv wires the lifecycle endpoints together with the resolver
// and the stats page, so scenarios can assert the full effect of a toggle:
// resolution stops/resumes, stats stay reachable.
func newArchiveRenewEnv(t *testing.T) *archiveRenewEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	ks := store.NewKeywordStore(db)
	us := store.NewUserStore(db)
	cs := store.NewClickStore(db)

	ctx := context.Background()
	owner, err := us.Upsert(ctx, "test", "ar-owner", "ar-owner@example.com", "Owner", "user")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	recipient, err := us.Upsert(ctx, "test", "ar-recipient", "ar-recipient@example.com", "Recipient", "user")
	if err != nil {
		t.Fatalf("seed recipient: %v", err)
	}
	stranger, err := us.Upsert(ctx, "test", "ar-stranger", "ar-stranger@example.com", "Stranger", "user")
	if err != nil {
		t.Fatalf("seed stranger: %v", err)
	}

	links := NewLinksHandler(ls, owns, us, ks)
	statsHandler := NewStatsHandler(ls, cs, owns)
	clickCh := make(chan store.ClickEvent, 8)
	resolver := NewResolveHandler(ls, ks, owns, clickCh)

	r := chi.NewRouter()
	r.Post("/dashboard/links/{id}/archive", links.Archive)
	r.Post("/dashboard/links/{id}/unarchive", links.Unarchive)
	r.Post("/dashboard/links/{id}/renew", links.Renew)
	r.Get("/dashboard/links/{id}", links.Detail)
	r.Get("/dashboard/links/{id}/stats", statsHandler.Show)
	r.Get("/{slug}*", resolver.Resolve)

	return &archiveRenewEnv{router: r, db: db, links: ls, clicks: cs, owner: owner, recipient: recipient, stranger: stranger}
}

// do issues a request as the given user; htmx toggles the HX-Request header.
func (e *archiveRenewEnv) do(t *testing.T, method, path string, user *store.User, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
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

// expireLink backdates expires_at directly — the write paths reject new past
// values, so tests reach the expired state the same way time does.
func (e *archiveRenewEnv) expireLink(t *testing.T, linkID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := e.db.Exec(e.db.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`), past, linkID); err != nil {
		t.Fatalf("expire link: %v", err)
	}
}

// clickCount counts link_clicks rows for a link.
func (e *archiveRenewEnv) clickCount(t *testing.T, linkID string) int {
	t.Helper()
	var n int
	if err := e.db.Get(&n, e.db.Rebind(`SELECT COUNT(*) FROM link_clicks WHERE link_id = ?`), linkID); err != nil {
		t.Fatalf("count clicks: %v", err)
	}
	return n
}

// Scenario: Archive Toggle Stops Resolution, Keeps Stats
// WHEN an owner archives a link that has recorded clicks
// THEN the link stops resolving, its link_clicks rows are unchanged, and its
// stats page remains accessible to the owner.
func TestWebArchive_ArchiveToggleStopsResolutionKeepsStats(t *testing.T) {
	env := newArchiveRenewEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "retire-me", "https://example.com/retire", env.owner.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	for range 2 {
		if err := env.clicks.RecordClick(ctx, store.ClickEvent{LinkID: link.ID}); err != nil {
			t.Fatalf("seed click: %v", err)
		}
	}
	if w := env.do(t, http.MethodGet, "/retire-me", nil, false); w.Code != http.StatusFound {
		t.Fatalf("precondition: resolve status = %d, want 302", w.Code)
	}

	if w := env.do(t, http.MethodPost, "/dashboard/links/"+link.ID+"/archive", env.owner, false); w.Code != http.StatusSeeOther {
		t.Fatalf("archive status = %d, want 303: %s", w.Code, w.Body.String())
	}

	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ArchivedAt == nil {
		t.Error("archived_at not set after archive toggle")
	}
	if w := env.do(t, http.MethodGet, "/retire-me", nil, false); w.Code != http.StatusNotFound {
		t.Errorf("archived link resolve status = %d, want 404", w.Code)
	}
	if n := env.clickCount(t, link.ID); n != 2 {
		t.Errorf("link_clicks rows after archive = %d, want 2 (archive never cascades clicks)", n)
	}
	if w := env.do(t, http.MethodGet, "/dashboard/links/"+link.ID+"/stats", env.owner, false); w.Code != http.StatusOK {
		t.Errorf("owner stats page after archive = %d, want 200", w.Code)
	}
}

// Scenario: Unarchive Restores Resolution
// WHEN an owner unarchives a link (and the link is not expired)
// THEN archived_at is cleared and /{slug} resolves with 302 Found again.
func TestWebArchive_UnarchiveRestoresResolution(t *testing.T) {
	env := newArchiveRenewEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "revive-me", "https://example.com/revive", env.owner.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if _, err := env.links.SetArchived(ctx, link.ID, true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if w := env.do(t, http.MethodGet, "/revive-me", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("precondition: archived resolve status = %d, want 404", w.Code)
	}

	// The endpoints are HTMX-aware: an HX request gets the refreshed detail
	// view swapped in place instead of a redirect.
	// Governing: SPEC-0020 REQ "Archive State" — toggle endpoints are HTMX-aware
	w := env.do(t, http.MethodPost, "/dashboard/links/"+link.ID+"/unarchive", env.owner, true)
	if w.Code != http.StatusOK {
		t.Fatalf("unarchive (HTMX) status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="link-detail"`) {
		t.Errorf("HTMX unarchive should render the detail fragment; body=%.300s", w.Body.String())
	}

	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ArchivedAt != nil {
		t.Errorf("archived_at after unarchive = %v, want nil", got.ArchivedAt)
	}
	w = env.do(t, http.MethodGet, "/revive-me", nil, false)
	if w.Code != http.StatusFound {
		t.Errorf("unarchived resolve status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://example.com/revive" {
		t.Errorf("Location = %q, want the destination", loc)
	}
}

// Edge pinned from PR #290 review: unarchiving a link whose expires_at is in
// the past clears archived_at but does NOT revive it — the derived state
// falls back to "expired" (Link.LifecycleState derivation) and resolution
// stays 404 until the link is renewed. "Unarchive Restores Resolution" only
// covers the non-expired case; this guards the fallback against regression.
// Governing: SPEC-0020 REQ "Archive State", REQ "Expired Link Resolution"
func TestWebArchive_UnarchiveOfExpiredLinkStaysExpired(t *testing.T) {
	env := newArchiveRenewEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "dormant", "https://example.com/dormant", env.owner.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	env.expireLink(t, link.ID)
	if _, err := env.links.SetArchived(ctx, link.ID, true); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if w := env.do(t, http.MethodPost, "/dashboard/links/"+link.ID+"/unarchive", env.owner, false); w.Code != http.StatusSeeOther {
		t.Fatalf("unarchive status = %d, want 303: %s", w.Code, w.Body.String())
	}

	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ArchivedAt != nil {
		t.Errorf("archived_at after unarchive = %v, want nil", got.ArchivedAt)
	}
	if state := got.LifecycleState(time.Now().UTC()); state != store.LifecycleExpired {
		t.Errorf("LifecycleState after unarchive = %q, want %q (expiry survives the archive round-trip)", state, store.LifecycleExpired)
	}
	if w := env.do(t, http.MethodGet, "/dormant", nil, false); w.Code != http.StatusNotFound {
		t.Errorf("expired-unarchived resolve status = %d, want 404 (unarchive does not revive an expired link)", w.Code)
	}
}

// Scenario: Non-Editor Cannot Archive
// WHEN a share recipient or unrelated user attempts the archive or unarchive
// action THEN the server responds 403 Forbidden and the link is unchanged.
func TestWebArchive_NonEditorCannotArchive(t *testing.T) {
	env := newArchiveRenewEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "guarded", "https://example.com/guarded", env.owner.ID, "", "", "secure")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := env.links.AddShare(ctx, link.ID, env.recipient.ID, env.owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	for _, user := range []*store.User{env.recipient, env.stranger} {
		if w := env.do(t, http.MethodPost, "/dashboard/links/"+link.ID+"/archive", user, false); w.Code != http.StatusForbidden {
			t.Errorf("archive as %s status = %d, want 403", user.Email, w.Code)
		}
	}
	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ArchivedAt != nil {
		t.Errorf("archived_at after forbidden archive = %v, want nil", got.ArchivedAt)
	}

	// Unarchive is gated identically.
	if _, err := env.links.SetArchived(ctx, link.ID, true); err != nil {
		t.Fatalf("archive as owner: %v", err)
	}
	for _, user := range []*store.User{env.recipient, env.stranger} {
		if w := env.do(t, http.MethodPost, "/dashboard/links/"+link.ID+"/unarchive", user, false); w.Code != http.StatusForbidden {
			t.Errorf("unarchive as %s status = %d, want 403", user.Email, w.Code)
		}
	}
	got, err = env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ArchivedAt == nil {
		t.Error("archived_at cleared by a forbidden unarchive")
	}
}

// Scenario: One-Click Renew Clears Expiry
// WHEN an owner clicks "Renew" on an expired link
// THEN expires_at becomes NULL, the link resolves again (absent archive), and
// the row re-renders without the expired badge.
func TestWebRenewal_OneClickRenewClearsExpiry(t *testing.T) {
	env := newArchiveRenewEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "renew-me", "https://example.com/renew", env.owner.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	env.expireLink(t, link.ID)
	if w := env.do(t, http.MethodGet, "/renew-me", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("precondition: expired resolve status = %d, want 404", w.Code)
	}

	w := env.do(t, http.MethodPost, "/dashboard/links/"+link.ID+"/renew", env.owner, true)
	if w.Code != http.StatusOK {
		t.Fatalf("renew (HTMX) status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="link-`+link.ID+`"`) {
		t.Errorf("renew must re-render the affected row; body=%.300s", body)
	}
	if strings.Contains(body, ">expired</span>") {
		t.Errorf("re-rendered row still carries the expired badge; body=%.500s", body)
	}
	if strings.Contains(body, "/dashboard/links/"+link.ID+"/renew") {
		t.Errorf("re-rendered row still offers the renew action; body=%.500s", body)
	}

	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after renew = %v, want nil", got.ExpiresAt)
	}
	if w := env.do(t, http.MethodGet, "/renew-me", nil, false); w.Code != http.StatusFound {
		t.Errorf("renewed resolve status = %d, want 302", w.Code)
	}
}

// Scenario: Renew Requires Edit Capability
// WHEN a share recipient invokes the renew action on an expired link shared
// with them THEN 403 Forbidden and the link remains expired.
func TestWebRenewal_RenewRequiresEditCapability(t *testing.T) {
	env := newArchiveRenewEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "shared-expired", "https://example.com/se", env.owner.ID, "", "", "secure")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := env.links.AddShare(ctx, link.ID, env.recipient.ID, env.owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}
	env.expireLink(t, link.ID)

	if w := env.do(t, http.MethodPost, "/dashboard/links/"+link.ID+"/renew", env.recipient, false); w.Code != http.StatusForbidden {
		t.Fatalf("renew as recipient status = %d, want 403", w.Code)
	}
	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExpiresAt == nil || !got.IsExpired(time.Now().UTC()) {
		t.Errorf("link no longer expired after forbidden renew: expires_at=%v", got.ExpiresAt)
	}
}

// Scenario: Renew Does Not Unarchive
// WHEN a link is both archived and expired and an owner renews it
// THEN expires_at is cleared but the link remains archived and does not resolve.
func TestWebRenewal_RenewDoesNotUnarchive(t *testing.T) {
	env := newArchiveRenewEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "both-gone", "https://example.com/both", env.owner.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	archived, err := env.links.SetArchived(ctx, link.ID, true)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	env.expireLink(t, link.ID)

	if w := env.do(t, http.MethodPost, "/dashboard/links/"+link.ID+"/renew", env.owner, false); w.Code != http.StatusSeeOther {
		t.Fatalf("renew status = %d, want 303: %s", w.Code, w.Body.String())
	}

	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after renew = %v, want nil", got.ExpiresAt)
	}
	if got.ArchivedAt == nil || !got.ArchivedAt.Equal(*archived.ArchivedAt) {
		t.Errorf("archived_at after renew = %v, want unchanged %v", got.ArchivedAt, archived.ArchivedAt)
	}
	if w := env.do(t, http.MethodGet, "/both-gone", nil, false); w.Code != http.StatusNotFound {
		t.Errorf("archived link resolve status after renew = %d, want 404", w.Code)
	}
}

// The renew button renders on two surfaces with different column sets: the
// dashboard (no Title column) and the tag detail page (ShowTitle=true —
// internal/handler/tags.go). The swapped-in fragment must match the shape of
// the table it lands in, or every cell after the missing Title <td> shifts
// under the wrong header until reload.
// Governing: SPEC-0020 REQ "Renewal" scenario "One-Click Renew Clears Expiry"
func TestWebRenewal_FragmentMatchesOriginSurfaceShape(t *testing.T) {
	env := newArchiveRenewEnv(t)
	ctx := context.Background()

	renew := func(t *testing.T, linkID, currentURL string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/dashboard/links/"+linkID+"/renew", nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, env.owner))
		req.Header.Set("HX-Request", "true")
		req.Header.Set("HX-Current-URL", currentURL)
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("renew status = %d, want 200: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	tagLink, err := env.links.Create(ctx, "tag-sourced", "https://example.com/t", env.owner.ID, "Tag Row Title", "", "public")
	if err != nil {
		t.Fatalf("seed tag-sourced: %v", err)
	}
	env.expireLink(t, tagLink.ID)
	body := renew(t, tagLink.ID, "https://go.example.com/dashboard/tags/ops")
	if !strings.Contains(body, "Tag Row Title") {
		t.Errorf("renew from the tag page dropped the Title cell; body=%.500s", body)
	}

	dashLink, err := env.links.Create(ctx, "dash-sourced", "https://example.com/d", env.owner.ID, "Dash Row Title", "", "public")
	if err != nil {
		t.Fatalf("seed dash-sourced: %v", err)
	}
	env.expireLink(t, dashLink.ID)
	body = renew(t, dashLink.ID, "https://go.example.com/dashboard")
	if strings.Contains(body, "Dash Row Title") {
		t.Errorf("renew from the dashboard must not add a Title cell; body=%.500s", body)
	}
}

// Scenario: Owner Sees Expired Badge on Dashboard (the renew-action half —
// the badge itself landed with PR #286)
// WHEN an owner views their dashboard and one of their links is expired
// THEN the link row appears with an "expired" badge and a renew action; a
// share recipient's row shows the badge but no renew action (CanEdit gate).
func TestDashboardRow_OwnerSeesExpiredBadgeAndRenewAction(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	link := &store.Link{
		ID:         "00000000-0000-0000-0000-00000000ab01",
		Slug:       "stale-row",
		URL:        "https://example.com/stale",
		Visibility: "public",
		ExpiresAt:  &past,
		CreatedAt:  time.Now().UTC(),
	}

	cases := []struct {
		name      string
		caps      store.LinkCaps
		wantRenew bool
	}{
		{"owner", store.NewLinkCaps(true, false, false), true},
		{"share-recipient", store.NewLinkCaps(false, true, false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
			ctx := DashboardPage{
				BasePage:       newBasePage(r, nil),
				ShowVisibility: true,
				ShowActions:    true,
				ShowLifecycle:  true,
				RowCaps:        map[string]store.LinkCaps{link.ID: tc.caps},
			}
			rr := httptest.NewRecorder()
			renderFragment(rr, "link_row", map[string]any{"Link": link, "Ctx": ctx})
			body := rr.Body.String()
			if !strings.Contains(body, ">expired</span>") {
				t.Fatalf("expired badge missing; body=%.500s", body)
			}
			gotRenew := strings.Contains(body, "/dashboard/links/"+link.ID+"/renew")
			if gotRenew != tc.wantRenew {
				t.Errorf("renew action rendered = %v, want %v; body=%.500s", gotRenew, tc.wantRenew, body)
			}
		})
	}
}
