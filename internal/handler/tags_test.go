package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

type tagsTestEnv struct {
	ls    *store.LinkStore
	h     *TagsHandler
	owner *store.User
	other *store.User
	admin *store.User
}

// newTagsTestEnv sets up a TagsHandler backed by an in-memory SQLite database
// with an owner, an unrelated authenticated user, and an admin.
func newTagsTestEnv(t *testing.T) *tagsTestEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	ks := store.NewKeywordStore(db)
	us := store.NewUserStore(db)
	ctx := context.Background()

	owner, err := us.Upsert(ctx, "test", "sub-owner", "owner@example.com", "Owner", "")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	other, err := us.Upsert(ctx, "test", "sub-other", "other@example.com", "Other", "")
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "sub-admin", "admin@example.com", "Admin", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	return &tagsTestEnv{
		ls:    ls,
		h:     NewTagsHandler(tags, ls, ks),
		owner: owner,
		other: other,
		admin: admin,
	}
}

// seedTaggedLink creates a link with the given visibility and tags.
func (e *tagsTestEnv) seedTaggedLink(t *testing.T, slug, visibility string, tagNames ...string) *store.Link {
	t.Helper()
	l, err := e.ls.Create(context.Background(), slug, "https://internal.example.com/"+slug, e.owner.ID, "", "", visibility)
	if err != nil {
		t.Fatalf("seed link %q: %v", slug, err)
	}
	if err := e.ls.SetTags(context.Background(), l.ID, tagNames); err != nil {
		t.Fatalf("tag link %q: %v", slug, err)
	}
	return l
}

// get routes a request through chi with the given user on the context.
func (e *tagsTestEnv) get(t *testing.T, path string, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if user != nil {
				req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/dashboard/tags", e.h.Index)
	r.Get("/dashboard/tags/{slug}", e.h.Detail)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — tag detail pages
// must not expose other users' private/secure links (issue #193).
func TestTagsDetail_HidesInvisibleLinksFromOtherUsers(t *testing.T) {
	env := newTagsTestEnv(t)
	env.seedTaggedLink(t, "payroll", "secure", "finance")
	env.seedTaggedLink(t, "handbook", "public", "finance")

	w := env.get(t, "/dashboard/tags/finance", env.other)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if strings.Contains(body, "payroll") || strings.Contains(body, "https://internal.example.com/payroll") {
		t.Errorf("secure link leaked to non-owner; body contains payroll slug/URL")
	}
	if !strings.Contains(body, "handbook") {
		t.Errorf("public link missing from tag page for non-owner")
	}
}

// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — owners still see
// their own private/secure links on tag pages.
func TestTagsDetail_OwnerSeesOwnLinks(t *testing.T) {
	env := newTagsTestEnv(t)
	env.seedTaggedLink(t, "payroll", "secure", "finance")

	w := env.get(t, "/dashboard/tags/finance", env.owner)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "payroll") {
		t.Errorf("owner should see their own secure link on the tag page")
	}
}

// Governing: SPEC-0010 REQ "Admin Visibility Override" — admins see all links.
func TestTagsDetail_AdminSeesAllLinks(t *testing.T) {
	env := newTagsTestEnv(t)
	env.seedTaggedLink(t, "payroll", "secure", "finance")

	w := env.get(t, "/dashboard/tags/finance", env.admin)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "payroll") {
		t.Errorf("admin should see secure links on the tag page")
	}
}

// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — a tag whose
// links are all invisible to the viewer must 404 exactly like a nonexistent
// tag, so slug probing cannot confirm sensitive tag names (tag-existence
// oracle; PR #241 security review).
func TestTagsDetail_AllInvisibleReturns404(t *testing.T) {
	env := newTagsTestEnv(t)
	env.seedTaggedLink(t, "payroll", "secure", "Layoffs 2026")

	for name, user := range map[string]*store.User{"other user": env.other, "anonymous": nil} {
		w := env.get(t, "/dashboard/tags/layoffs-2026", user)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want %d (tag-existence oracle)", name, w.Code, http.StatusNotFound)
		}
		if strings.Contains(w.Body.String(), "Layoffs 2026") {
			t.Errorf("%s: 404 body leaked the tag's display name", name)
		}
	}

	// Identical to a tag that does not exist at all.
	if w := env.get(t, "/dashboard/tags/no-such-tag", env.other); w.Code != http.StatusNotFound {
		t.Errorf("nonexistent tag: status = %d, want %d", w.Code, http.StatusNotFound)
	}

	// Owner and admin still get the page.
	for name, user := range map[string]*store.User{"owner": env.owner, "admin": env.admin} {
		if w := env.get(t, "/dashboard/tags/layoffs-2026", user); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want %d", name, w.Code, http.StatusOK)
		}
	}
}

// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — the tag index
// must not surface tags whose links are all invisible to the viewer.
func TestTagsIndex_HidesInvisibleTagsFromOtherUsers(t *testing.T) {
	env := newTagsTestEnv(t)
	env.seedTaggedLink(t, "payroll", "secure", "secret-projects")
	env.seedTaggedLink(t, "handbook", "public", "onboarding")

	w := env.get(t, "/dashboard/tags", env.other)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if strings.Contains(body, "secret-projects") {
		t.Errorf("tag with only invisible links leaked to non-owner")
	}
	if !strings.Contains(body, "onboarding") {
		t.Errorf("tag with public links missing from index")
	}

	// Admin still sees everything.
	w = env.get(t, "/dashboard/tags", env.admin)
	if !strings.Contains(w.Body.String(), "secret-projects") {
		t.Errorf("admin should see all tags on the index")
	}
}
