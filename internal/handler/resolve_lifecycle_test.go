// Lifecycle resolution semantics: expired and archived links at the resolver,
// plus the owner-facing dashboard badge. One test per spec scenario, named
// after it, so the spec↔test mapping is auditable.
// Governing: SPEC-0020 REQ "Expired Link Resolution", REQ "Archived Link Resolution", ADR-0020
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
	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

type lifecycleTestEnv struct {
	db      *sqlx.DB
	ls      *store.LinkStore
	owns    *store.OwnershipStore
	tags    *store.TagStore
	ks      *store.KeywordStore
	rh      *ResolveHandler
	owner   *store.User
	clickCh chan store.ClickEvent
}

// newLifecycleTestEnv builds a ResolveHandler with a buffered click channel so
// scenarios can assert that terminal lifecycle outcomes record no click event.
func newLifecycleTestEnv(t *testing.T) *lifecycleTestEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	ks := store.NewKeywordStore(db)
	us := store.NewUserStore(db)

	owner, err := us.Upsert(context.Background(), "test", "owner-sub", "owner@example.com", "Olive Owner", "")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	clickCh := make(chan store.ClickEvent, 8)
	rh := NewResolveHandler(ls, ks, owns, clickCh)
	return &lifecycleTestEnv{db: db, ls: ls, owns: owns, tags: tags, ks: ks, rh: rh, owner: owner, clickCh: clickCh}
}

// seedLink creates a link owned by env.owner with the given visibility.
func (e *lifecycleTestEnv) seedLink(t *testing.T, slug, target, visibility string) *store.Link {
	t.Helper()
	l, err := e.ls.Create(context.Background(), slug, target, e.owner.ID, "", "", visibility)
	if err != nil {
		t.Fatalf("seed link %q: %v", slug, err)
	}
	return l
}

// expire backdates the link's expires_at directly — the write paths reject
// past values (SPEC-0020 REQ "Link Expiration"), so tests reach the expired
// state the same way time does: after the fact.
func (e *lifecycleTestEnv) expire(t *testing.T, linkID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := e.db.Exec(e.db.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`), past, linkID); err != nil {
		t.Fatalf("expire link: %v", err)
	}
}

// archive sets archived_at directly (the archive toggle endpoint is issue #273).
func (e *lifecycleTestEnv) archive(t *testing.T, linkID string) {
	t.Helper()
	if _, err := e.db.Exec(e.db.Rebind(`UPDATE links SET archived_at = ? WHERE id = ?`), time.Now().UTC(), linkID); err != nil {
		t.Fatalf("archive link: %v", err)
	}
}

// resolveAs routes a GET through the resolver, optionally authenticated.
func (e *lifecycleTestEnv) resolveAs(t *testing.T, path string, user *store.User, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/{slug}*", e.rh.Resolve)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertNoClick fails if any click event was recorded — expired and archived
// resolutions never redirect and never count as clicks.
// Governing: SPEC-0020 REQ "Expired Link Resolution", REQ "Archived Link Resolution"
func (e *lifecycleTestEnv) assertNoClick(t *testing.T) {
	t.Helper()
	select {
	case ev := <-e.clickCh:
		t.Errorf("click event recorded for terminal lifecycle resolution: %+v", ev)
	default:
	}
}

// Scenario: Expired Public Link Shows Expired Page
// Governing: SPEC-0020 REQ "Expired Link Resolution"
func TestResolveLifecycle_ExpiredPublicLinkShowsExpiredPage(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "sprint", "https://example.com/sprint", "public")
	env.expire(t, l.ID)

	w := env.resolveAs(t, "/sprint", nil, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("expired link redirected to %q, want no redirect", loc)
	}
	body := w.Body.String()
	if !strings.Contains(body, "expired") || !strings.Contains(body, "sprint") {
		t.Errorf("expired page must name the slug and say expired; body=%s", body)
	}
	// Owner is named for a public link, linking to the public profile (SPEC-0012).
	if !strings.Contains(body, "Olive Owner") || !strings.Contains(body, "/u/olive-owner") {
		t.Errorf("expired public page must name the owner with a profile link; body=%s", body)
	}
	if strings.Contains(body, "Create this link") || strings.Contains(body, "Sign in to create") {
		t.Errorf("expired page must not offer the Create CTA; body=%s", body)
	}
	env.assertNoClick(t)

	// HX-Request renders the same content as a fragment (SPEC-0004 conventions).
	wf := env.resolveAs(t, "/sprint", nil, true)
	if wf.Code != http.StatusNotFound {
		t.Fatalf("HTMX status = %d, want %d", wf.Code, http.StatusNotFound)
	}
	frag := wf.Body.String()
	if !strings.Contains(frag, "expired") || strings.Contains(frag, "<html") {
		t.Errorf("HTMX request must render the expired content as a fragment; body=%s", frag)
	}
}

// Scenario: Expired Secure Link — Anonymous Visitor Learns Nothing
// Governing: SPEC-0020 REQ "Expired Link Resolution", Security "Resolution Ordering and Oracle Resistance"
func TestResolveLifecycle_ExpiredSecureLinkAnonymousVisitorLearnsNothing(t *testing.T) {
	env := newLifecycleTestEnv(t)
	expired := env.seedLink(t, "secret-expired", "https://internal.example.com/a", "secure")
	env.expire(t, expired.ID)
	env.seedLink(t, "secret-active", "https://internal.example.com/b", "secure")

	wExpired := env.resolveAs(t, "/secret-expired", nil, false)
	wActive := env.resolveAs(t, "/secret-active", nil, false)

	if wExpired.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (login redirect)", wExpired.Code, http.StatusFound)
	}
	want := "/auth/login?return_url=" + url.QueryEscape("/secret-expired")
	if loc := wExpired.Header().Get("Location"); loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
	if strings.Contains(wExpired.Body.String(), "Link expired") {
		t.Errorf("anonymous response must not disclose expiry; body=%s", wExpired.Body.String())
	}
	// Oracle parity: the response shape is identical to an active secure link —
	// same status, same redirect target modulo the slug.
	if wExpired.Code != wActive.Code {
		t.Errorf("expired secure status %d differs from active secure status %d", wExpired.Code, wActive.Code)
	}
	env.assertNoClick(t)
}

// Scenario: Expired Secure Link — Authorized Viewer Sees Expiry
// Governing: SPEC-0020 REQ "Expired Link Resolution"
func TestResolveLifecycle_ExpiredSecureLinkAuthorizedViewerSeesExpiry(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "secret", "https://internal.example.com/doc", "secure")
	env.expire(t, l.ID)

	// Owner sees the expired page.
	w := env.resolveAs(t, "/secret", env.owner, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("owner status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if body := w.Body.String(); !strings.Contains(body, "expired") {
		t.Errorf("owner must see the expired page; body=%s", body)
	}

	// A share recipient passes the visibility gate too and sees the expired
	// page (owner MAY be named — the viewer holds CanView).
	us := store.NewUserStore(env.db)
	recipient, err := us.Upsert(context.Background(), "test", "recipient-sub", "r@example.com", "Rae Recipient", "")
	if err != nil {
		t.Fatalf("seed recipient: %v", err)
	}
	if err := env.ls.AddShare(context.Background(), l.ID, recipient.ID, env.owner.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}
	wr := env.resolveAs(t, "/secret", recipient, false)
	if wr.Code != http.StatusNotFound {
		t.Fatalf("recipient status = %d, want %d", wr.Code, http.StatusNotFound)
	}
	if body := wr.Body.String(); !strings.Contains(body, "expired") || !strings.Contains(body, "Olive Owner") {
		t.Errorf("share recipient holds CanView and sees expiry with owner named; body=%s", body)
	}
	env.assertNoClick(t)
}

// Unauthorized authenticated users still get the styled 403, exactly as for an
// active secure link — no expiry disclosure (Resolution Ordering).
// Governing: SPEC-0020 Security "Resolution Ordering and Oracle Resistance"
func TestResolveLifecycle_ExpiredSecureLinkUnauthorizedUserGets403(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "secret", "https://internal.example.com/doc", "secure")
	env.expire(t, l.ID)

	us := store.NewUserStore(env.db)
	outsider, err := us.Upsert(context.Background(), "test", "outsider-sub", "o@example.com", "Otto Outsider", "")
	if err != nil {
		t.Fatalf("seed outsider: %v", err)
	}
	w := env.resolveAs(t, "/secret", outsider, false)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if strings.Contains(w.Body.String(), "expired") {
		t.Errorf("403 must not disclose expiry; body=%s", w.Body.String())
	}
	env.assertNoClick(t)
}

// The expired page names the owner only when the link is public or the viewer
// holds CanView — other viewers of a private expired link get no owner identity.
// Governing: SPEC-0020 REQ "Expired Link Resolution"
func TestResolveLifecycle_ExpiredPrivateLinkOmitsOwnerIdentity(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "hush", "https://example.com/hush", "private")
	env.expire(t, l.ID)

	// Anonymous slug-holder: expired page renders (private resolves for anyone
	// presenting the slug) but without owner identity.
	w := env.resolveAs(t, "/hush", nil, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	if !strings.Contains(body, "expired") {
		t.Errorf("private expired link must render the expired page; body=%s", body)
	}
	if strings.Contains(body, "Olive Owner") || strings.Contains(body, "/u/olive-owner") {
		t.Errorf("private expired page must omit owner identity for non-CanView viewers; body=%s", body)
	}

	// The owner holds CanView and is named.
	wo := env.resolveAs(t, "/hush", env.owner, false)
	if !strings.Contains(wo.Body.String(), "Olive Owner") {
		t.Errorf("CanView viewer of a private expired link sees the owner; body=%s", wo.Body.String())
	}
}

// Scenario: Expired Prefix Match Terminates Resolution
// Governing: SPEC-0020 REQ "Expired Link Resolution"
func TestResolveLifecycle_ExpiredPrefixMatchTerminatesResolution(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "jira", "https://jira.example.com/browse/$ticket", "public")
	env.expire(t, l.ID)

	w := env.resolveAs(t, "/jira/PROJ-1", nil, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	// The expired page names the matched slug "jira", not the full path.
	if !strings.Contains(body, "expired") || !strings.Contains(body, ">jira<") {
		t.Errorf("expired prefix match must render the expired page naming the matched slug; body=%s", body)
	}
	if strings.Contains(body, "jira/PROJ-1") {
		t.Errorf("expired page must name the matched slug, not the full requested path; body=%s", body)
	}
	env.assertNoClick(t)
}

// A longer expired prefix commits the resolver — it never falls through to a
// shorter, still-active prefix.
// Governing: SPEC-0020 REQ "Expired Link Resolution" — no fall-through to shorter prefixes
func TestResolveLifecycle_ExpiredPrefixDoesNotFallThroughToShorterPrefix(t *testing.T) {
	env := newLifecycleTestEnv(t)
	env.seedLink(t, "docs", "https://docs.example.com/$page", "public")
	// Multi-segment slug rows exist via legacy/import paths (SPEC-0009); seed raw.
	if _, err := env.db.Exec(env.db.Rebind(
		`INSERT INTO links (id, slug, url, title, description, visibility, expires_at) VALUES (?, ?, ?, '', '', 'public', ?)`),
		"docs-api-raw-id", "docs/api", "https://api.example.com", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("seed raw link: %v", err)
	}

	w := env.resolveAs(t, "/docs/api/tokens", nil, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d — expired longest prefix must terminate resolution", w.Code, http.StatusNotFound)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("resolver fell through to shorter prefix and redirected to %q", loc)
	}
	if body := w.Body.String(); !strings.Contains(body, "expired") {
		t.Errorf("want expired page for the committed prefix match; body=%s", body)
	}
}

// An archived prefix match commits the resolver the same way an expired one
// does: the archived-404 outcome (standard 404, no CTA, naming the matched
// slug), no fall-through, no click. The code path is shared with the expired
// variant (checkLifecycle inside the prefix loop), but this pins the archived
// branch against a future special-casing regression.
// Governing: SPEC-0020 REQ "Expired Link Resolution" — "expired or archived" prefix matches
func TestResolveLifecycle_ArchivedPrefixMatchTerminatesResolution(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "jira", "https://jira.example.com/browse/$ticket", "public")
	env.archive(t, l.ID)

	w := env.resolveAs(t, "/jira/PROJ-1", nil, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("resolver fell through past the archived prefix and redirected to %q", loc)
	}
	body := w.Body.String()
	// Archived behavior: the standard 404 naming the matched slug, no
	// lifecycle disclosure, no Create CTA (the slug stays reserved).
	if !strings.Contains(body, "Link not found") || !strings.Contains(body, ">jira<") {
		t.Errorf("archived prefix match must render the standard 404 naming the matched slug; body=%s", body)
	}
	if strings.Contains(body, "jira/PROJ-1") {
		t.Errorf("404 must name the matched slug, not the full requested path; body=%s", body)
	}
	if strings.Contains(body, "archived") || strings.Contains(body, "expired") {
		t.Errorf("archived 404 must not disclose lifecycle state; body=%s", body)
	}
	if strings.Contains(body, "Create this link") || strings.Contains(body, "Sign in to create") {
		t.Errorf("archived 404 must not offer the Create CTA; body=%s", body)
	}
	env.assertNoClick(t)
}

// Scenario: Owner Sees Expired Badge on Dashboard
// (The renew action on the row ships with the archive/renew endpoints — #273.)
// Governing: SPEC-0020 REQ "Expired Link Resolution"
func TestResolveLifecycle_OwnerSeesExpiredBadgeOnDashboard(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "old-report", "https://example.com/report", "public")
	env.expire(t, l.ID)
	env.seedLink(t, "fresh", "https://example.com/fresh", "public")

	dh := NewDashboardHandler(env.ls, env.owns, env.tags, env.ks)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, env.owner))
	w := httptest.NewRecorder()
	dh.Show(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !strings.Contains(body, "old-report") {
		t.Fatalf("expired link must still appear on the owner dashboard; body=%s", body)
	}
	if !strings.Contains(body, `>expired</span>`) {
		t.Errorf("expired row must carry an expired badge; body=%s", body)
	}
	// The active link's row must not be badged: exactly one expired badge.
	if got := strings.Count(body, `>expired</span>`); got != 1 {
		t.Errorf("expired badge count = %d, want 1", got)
	}
}

// Scenario: Archived Public Link Renders 404 Without Create CTA
// Governing: SPEC-0020 REQ "Archived Link Resolution"
func TestResolveLifecycle_ArchivedPublicLinkRenders404WithoutCreateCTA(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "retired", "https://example.com/retired", "public")
	env.archive(t, l.ID)

	w := env.resolveAs(t, "/retired", nil, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	// The standard 404 page, not a distinct "archived" page.
	if !strings.Contains(body, "Link not found") {
		t.Errorf("archived link must render the standard 404 page; body=%s", body)
	}
	if strings.Contains(body, "archived") || strings.Contains(body, "expired") {
		t.Errorf("archived 404 must not disclose lifecycle state; body=%s", body)
	}
	if strings.Contains(body, "Create this link") || strings.Contains(body, "Sign in to create") {
		t.Errorf("archived 404 must not offer the Create CTA — the slug stays reserved; body=%s", body)
	}
	env.assertNoClick(t)

	// Control: a genuinely free slug still offers the CTA, so the suppression
	// above is the archived state, not a broken 404 page.
	wc := env.resolveAs(t, "/free-slug", nil, false)
	if !strings.Contains(wc.Body.String(), "Sign in to create") {
		t.Fatalf("control: free slug 404 should offer the CTA; body=%s", wc.Body.String())
	}
}

// Scenario: Archived Secure Link — Anonymous Visitor Learns Nothing
// Governing: SPEC-0020 REQ "Archived Link Resolution", Security "Resolution Ordering and Oracle Resistance"
func TestResolveLifecycle_ArchivedSecureLinkAnonymousVisitorLearnsNothing(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "vault", "https://internal.example.com/vault", "secure")
	env.archive(t, l.ID)

	w := env.resolveAs(t, "/vault", nil, false)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (login redirect)", w.Code, http.StatusFound)
	}
	want := "/auth/login?return_url=" + url.QueryEscape("/vault")
	if loc := w.Header().Get("Location"); loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
	env.assertNoClick(t)
}

// Scenario: Archived Beats Expired in Derived State
// Governing: SPEC-0020 REQ "Archived Link Resolution"
func TestResolveLifecycle_ArchivedBeatsExpiredInDerivedState(t *testing.T) {
	env := newLifecycleTestEnv(t)
	l := env.seedLink(t, "both", "https://example.com/both", "public")
	env.expire(t, l.ID)
	env.archive(t, l.ID)

	w := env.resolveAs(t, "/both", nil, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	body := w.Body.String()
	// Archived behavior: the standard 404 page, never the expired page.
	if !strings.Contains(body, "Link not found") {
		t.Errorf("archived+expired link must follow archived behavior (standard 404); body=%s", body)
	}
	if strings.Contains(body, "expired") {
		t.Errorf("archived must beat expired — no expired page content; body=%s", body)
	}
	env.assertNoClick(t)
}
