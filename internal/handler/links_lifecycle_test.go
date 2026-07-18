// Web-form tests for the SPEC-0020 REQ "Link Expiration" scenarios: the
// optional expiration input on the create and edit forms, past-value
// rejection, round-trip editability of expired links, clearing on edit, and
// the capability gate keeping share recipients away from expiry. Tests are
// named after the spec scenarios so the spec↔test mapping is auditable.
//
// Governing: SPEC-0020 REQ "Link Expiration", ADR-0020
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

type lifecycleEnv struct {
	router    http.Handler
	db        *sqlx.DB
	links     *store.LinkStore
	owner     *store.User
	recipient *store.User
}

func newLifecycleEnv(t *testing.T) *lifecycleEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	ks := store.NewKeywordStore(db)
	us := store.NewUserStore(db)

	ctx := context.Background()
	owner, err := us.Upsert(ctx, "test", "lc-owner", "lc-owner@example.com", "Owner", "user")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	recipient, err := us.Upsert(ctx, "test", "lc-recipient", "lc-recipient@example.com", "Recipient", "user")
	if err != nil {
		t.Fatalf("seed recipient: %v", err)
	}

	links := NewLinksHandler(ls, owns, us, ks)
	r := chi.NewRouter()
	r.Post("/dashboard/links", links.Create)
	r.Get("/dashboard/links/{id}/edit", links.Edit)
	r.Put("/dashboard/links/{id}", links.Update)

	return &lifecycleEnv{router: r, db: db, links: ls, owner: owner, recipient: recipient}
}

// submit posts form values as the given user.
func (e *lifecycleEnv) submit(t *testing.T, method, path string, form url.Values, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// Scenario: Link Created with Expiration
// WHEN a user creates a link with the expiration input set to a future time
// THEN the link is persisted with that expires_at.
func TestWebFormLifecycle_LinkCreatedWithExpiration(t *testing.T) {
	env := newLifecycleEnv(t)

	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	w := env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
		"slug":       {"expires-web"},
		"url":        {"https://example.com"},
		"expires_at": {future.Format("2006-01-02T15:04:05")},
	}, env.owner)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303: %s", w.Code, w.Body.String())
	}

	link, err := env.links.GetBySlug(context.Background(), "expires-web")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if link.ExpiresAt == nil || !link.ExpiresAt.Equal(future) {
		t.Errorf("expires_at = %v, want %v", link.ExpiresAt, future)
	}

	// The expiration input is optional: empty means never expires.
	w = env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
		"slug":       {"forever-web"},
		"url":        {"https://example.com/2"},
		"expires_at": {""},
	}, env.owner)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303", w.Code)
	}
	plain, err := env.links.GetBySlug(context.Background(), "forever-web")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if plain.ExpiresAt != nil {
		t.Errorf("expires_at with empty input = %v, want nil", plain.ExpiresAt)
	}
}

// Scenario: Past Expiration Rejected
// WHEN the create or edit form submits a past expiration that differs from
// the stored value THEN the form re-renders with a validation error and
// nothing is persisted.
func TestWebFormLifecycle_PastExpirationRejected(t *testing.T) {
	env := newLifecycleEnv(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05")
	w := env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
		"slug":       {"born-dead-web"},
		"url":        {"https://example.com"},
		"expires_at": {past},
	}, env.owner)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 re-render", w.Code)
	}
	if !strings.Contains(w.Body.String(), "expiration must be in the future") {
		t.Errorf("form missing validation error; body=%.300s", w.Body.String())
	}
	if _, err := env.links.GetBySlug(ctx, "born-dead-web"); err == nil {
		t.Error("link was created despite past expiration")
	}

	link, err := env.links.Create(ctx, "alive-web", "https://example.com", env.owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	w = env.submit(t, http.MethodPut, "/dashboard/links/"+link.ID, url.Values{
		"url":        {"https://example.com"},
		"expires_at": {past},
	}, env.owner)
	if w.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200 re-render", w.Code)
	}
	if !strings.Contains(w.Body.String(), "expiration must be in the future") {
		t.Errorf("edit form missing validation error; body=%.300s", w.Body.String())
	}
	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after rejected edit = %v, want nil", got.ExpiresAt)
	}
}

// Scenario: Expired Link Stays Editable
// WHEN an owner edits the title of an expired link and the form round-trips
// the stored (past) expires_at unchanged THEN the update succeeds and
// expires_at keeps its stored value.
func TestWebFormLifecycle_ExpiredLinkStaysEditable(t *testing.T) {
	env := newLifecycleEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "expired-web", "https://example.com", env.owner.ID, "Old", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	past := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	if _, err := env.db.Exec(env.db.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`), past, link.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}

	// The edit form pre-fills the stored value ("clearable on edit" renders it).
	w := env.submit(t, http.MethodGet, "/dashboard/links/"+link.ID+"/edit", nil, env.owner)
	if w.Code != http.StatusOK {
		t.Fatalf("edit form status = %d, want 200", w.Code)
	}
	rendered := past.Format("2006-01-02T15:04:05")
	if !strings.Contains(w.Body.String(), `value="`+rendered+`"`) {
		t.Errorf("edit form does not pre-fill stored expires_at %q", rendered)
	}

	// Submitting the form back with only the title changed round-trips the
	// past value and must succeed.
	w = env.submit(t, http.MethodPut, "/dashboard/links/"+link.ID, url.Values{
		"url":        {"https://example.com"},
		"title":      {"New"},
		"expires_at": {rendered},
	}, env.owner)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("round-trip edit status = %d, want 303: %.300s", w.Code, w.Body.String())
	}
	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Title != "New" {
		t.Errorf("title = %q, want %q", got.Title, "New")
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(past) {
		t.Errorf("expires_at = %v, want unchanged %v", got.ExpiresAt, past)
	}
}

// Scenario: Expiration Cleared on Edit
// WHEN an owner clears the expiration field THEN stored expires_at becomes
// NULL and the link no longer expires.
func TestWebFormLifecycle_ExpirationClearedOnEdit(t *testing.T) {
	env := newLifecycleEnv(t)
	ctx := context.Background()

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	link, err := env.links.CreateFull(ctx, "clear-web", "https://example.com", env.owner.ID, "", "", "public", &future, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	w := env.submit(t, http.MethodPut, "/dashboard/links/"+link.ID, url.Values{
		"url":        {"https://example.com"},
		"expires_at": {""},
	}, env.owner)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("clear edit status = %d, want 303: %.300s", w.Code, w.Body.String())
	}
	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after clear = %v, want nil", got.ExpiresAt)
	}
}

// Scenario: Share Recipient Cannot Set Expiry
// WHEN a user whose only relationship to the link is a link_shares record
// submits the edit form with an expiration THEN 403 Forbidden and no change.
func TestWebFormLifecycle_ShareRecipientCannotSetExpiry(t *testing.T) {
	env := newLifecycleEnv(t)
	ctx := context.Background()

	link, err := env.links.Create(ctx, "shared-web", "https://example.com", env.owner.ID, "", "", "secure")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := env.links.AddShare(ctx, link.ID, env.recipient.ID, env.owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	future := time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05")
	w := env.submit(t, http.MethodPut, "/dashboard/links/"+link.ID, url.Values{
		"url":        {"https://example.com"},
		"expires_at": {future},
	}, env.recipient)
	if w.Code != http.StatusForbidden {
		t.Fatalf("recipient edit status = %d, want 403", w.Code)
	}
	got, err := env.links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after forbidden edit = %v, want nil", got.ExpiresAt)
	}
}
