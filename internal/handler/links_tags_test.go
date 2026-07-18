// Tag handling on the create/edit link forms: duplicate spellings of the same
// tag must be deduped instead of silently losing every tag, and a failed tag
// write must re-render the form with an error instead of being swallowed
// (issue #198).
//
// Governing: SPEC-0004 REQ "New Link Form", REQ "Edit Link Form"
package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

type linkFormEnv struct {
	db     *sqlx.DB
	ls     *store.LinkStore
	router http.Handler
	user   *store.User
}

// newLinkFormEnv wires a LinksHandler with the create/update routes used by
// the dashboard forms.
func newLinkFormEnv(t *testing.T) *linkFormEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	ks := store.NewKeywordStore(db)
	us := store.NewUserStore(db)

	user, err := us.Upsert(context.Background(), "test", "form-user", "form-user@example.com", "Form User", "user")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	links := NewLinksHandler(ls, owns, us, ks)
	r := chi.NewRouter()
	r.Post("/dashboard/links", links.Create)
	r.Put("/dashboard/links/{id}", links.Update)

	return &linkFormEnv{db: db, ls: ls, router: r, user: user}
}

// submit posts a form-encoded request as the env user.
func (e *linkFormEnv) submit(t *testing.T, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, e.user))
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// Creating a link with case-duplicate tags must succeed and attach a single
// deduped tag rather than rolling back the tag write (issue #198).
func TestLinksCreate_DuplicateTagsDeduped(t *testing.T) {
	env := newLinkFormEnv(t)

	w := env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
		"slug": {"jira-board"},
		"url":  {"https://example.com/jira"},
		"tags": {"Jira, jira"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusSeeOther, w.Body.String())
	}

	link, err := env.ls.GetBySlug(context.Background(), "jira-board")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	tags, err := env.ls.ListTags(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Name != "Jira" {
		t.Errorf("tags = %+v, want exactly one tag named Jira", tags)
	}
}

// When the tag write fails during create, the whole create rolls back and the
// form re-renders with an error — no silent tag loss, no half-created link
// (issue #198).
func TestLinksCreate_TagWriteFailureRendersFormError(t *testing.T) {
	env := newLinkFormEnv(t)
	// Force every link_tags write to fail.
	env.db.MustExec("DROP TABLE link_tags")

	w := env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
		"slug": {"doomed-link"},
		"url":  {"https://example.com/doomed"},
		"tags": {"jira"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (form re-render)", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "Could not create the link") {
		t.Errorf("form error message missing from body: %s", w.Body.String())
	}

	// Atomic: no half-created link left behind.
	if _, err := env.ls.GetBySlug(context.Background(), "doomed-link"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug after failed create = %v, want ErrNotFound", err)
	}
}

// Updating a link with case-duplicate tags must dedupe rather than silently
// dropping the tag update (issue #198).
func TestLinksUpdate_DuplicateTagsDeduped(t *testing.T) {
	env := newLinkFormEnv(t)
	link, err := env.ls.Create(context.Background(), "edit-me", "https://example.com", env.user.ID, "", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	w := env.submit(t, http.MethodPut, "/dashboard/links/"+link.ID, url.Values{
		"url":  {"https://example.com"},
		"tags": {"Jira, jira, JIRA"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusSeeOther, w.Body.String())
	}

	tags, err := env.ls.ListTags(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Name != "Jira" {
		t.Errorf("tags = %+v, want exactly one tag named Jira", tags)
	}
}

// Hostile tag names must be rejected at intake with a form error — defense in
// depth for the stored XSS class fixed at the output layer in #246 (issue #251).
func TestLinksCreate_HostileTagNameRejected(t *testing.T) {
	env := newLinkFormEnv(t)

	w := env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
		"slug": {"evil-tags"},
		"url":  {"https://example.com"},
		"tags": {`x');fetch('/evil')//`},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (form re-render)", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "tag names must start with a letter or digit") {
		t.Errorf("tag validation error missing from body: %s", w.Body.String())
	}
	// Nothing was written.
	if _, err := env.ls.GetBySlug(context.Background(), "evil-tags"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug after rejected create = %v, want ErrNotFound", err)
	}
}

// javascript:/data: URLs must be rejected at intake on the web form —
// they are stored-XSS primitives on any redirect surface (issue #265).
func TestLinksCreate_NonHTTPSchemeRejected(t *testing.T) {
	env := newLinkFormEnv(t)

	for _, badURL := range []string{"javascript:alert(1)", "data:text/html,x"} {
		w := env.submit(t, http.MethodPost, "/dashboard/links", url.Values{
			"slug": {"evil-url"},
			"url":  {badURL},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want %d (form re-render)", badURL, w.Code, http.StatusOK)
		}
		if !strings.Contains(w.Body.String(), "url must start with http") {
			t.Errorf("%s: scheme error message missing from body: %s", badURL, w.Body.String())
		}
		if _, err := env.ls.GetBySlug(context.Background(), "evil-url"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: GetBySlug after rejected create = %v, want ErrNotFound", badURL, err)
		}
	}
}

// Update must reject hostile tag names BEFORE the link row is written, so a
// bad tag cannot leave the link half-updated (issues #251, #265).
func TestLinksUpdate_HostileTagNameRejectedBeforeWrite(t *testing.T) {
	env := newLinkFormEnv(t)
	link, err := env.ls.Create(context.Background(), "edit-hostile", "https://example.com", env.user.ID, "Old Title", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	w := env.submit(t, http.MethodPut, "/dashboard/links/"+link.ID, url.Values{
		"url":   {"https://example.com/new"},
		"title": {"New Title"},
		"tags":  {`x');fetch('/evil')//`},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (form re-render)", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "tag names must start with a letter or digit") {
		t.Errorf("tag validation error missing from body: %s", w.Body.String())
	}

	got, err := env.ls.GetByID(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("reload link: %v", err)
	}
	if got.URL != "https://example.com" || got.Title != "Old Title" {
		t.Errorf("link row modified by rejected update: url=%q title=%q", got.URL, got.Title)
	}
}

// Update must reject javascript: URLs with a form error (issue #265).
func TestLinksUpdate_NonHTTPSchemeRejected(t *testing.T) {
	env := newLinkFormEnv(t)
	link, err := env.ls.Create(context.Background(), "edit-scheme", "https://example.com", env.user.ID, "", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	w := env.submit(t, http.MethodPut, "/dashboard/links/"+link.ID, url.Values{
		"url": {"javascript:alert(1)"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (form re-render)", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "url must start with http") {
		t.Errorf("scheme error message missing from body: %s", w.Body.String())
	}

	got, err := env.ls.GetByID(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("reload link: %v", err)
	}
	if got.URL != "https://example.com" {
		t.Errorf("url = %q, want unchanged https://example.com", got.URL)
	}
}

// When the tag write fails during update, the edit form re-renders with an
// error telling the user the tags were not saved (issue #198).
func TestLinksUpdate_TagWriteFailureRendersFormError(t *testing.T) {
	env := newLinkFormEnv(t)
	link, err := env.ls.Create(context.Background(), "edit-fail", "https://example.com", env.user.ID, "", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	// Force every link_tags write to fail.
	env.db.MustExec("DROP TABLE link_tags")

	w := env.submit(t, http.MethodPut, "/dashboard/links/"+link.ID, url.Values{
		"url":  {"https://example.com/updated"},
		"tags": {"jira"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (form re-render)", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "tags could not be updated") {
		t.Errorf("form error message missing from body: %s", w.Body.String())
	}
}
