// Governing: SPEC-0001 REQ "Role-Based Access Control", ADR-0003
// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
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

type forbiddenTestEnv struct {
	router http.Handler
	owner  *store.User
	other  *store.User
	admin  *store.User
	linkID string
}

// newForbiddenTestEnv builds real stores on an in-memory SQLite database and
// a router that mirrors NewRouter's wiring for the web-UI routes with
// forbidden paths: /admin/* behind RequireRole (with the injected styled
// renderer) and the ownership-guarded link routes.
func newForbiddenTestEnv(t *testing.T) *forbiddenTestEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	ks := store.NewKeywordStore(db)
	us := store.NewUserStore(db)
	cs := store.NewClickStore(db)

	ctx := context.Background()
	owner, err := us.Upsert(ctx, "test", "owner", "owner@example.com", "Owner", "user")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	other, err := us.Upsert(ctx, "test", "other", "other@example.com", "Other", "user")
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	link, err := ls.Create(ctx, "example", "https://example.com", owner.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	links := NewLinksHandler(ls, owns, us, ks)
	statsHandler := NewStatsHandler(ls, cs, owns, 0)
	keywordsHandler := NewKeywordsHandler(ks)

	// Mirror NewRouter: inject the styled forbidden renderer into the auth
	// middleware for RequireRole failures.
	authMW := auth.NewMiddleware(nil, nil)
	authMW.Forbidden = RenderForbidden

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(authMW.RequireRole("admin"))
		r.Get("/admin/keywords", keywordsHandler.Index)
	})
	r.Get("/dashboard/links/{id}", links.Detail)
	r.Get("/dashboard/links/{id}/edit", links.Edit)
	r.Get("/dashboard/links/{id}/stats", statsHandler.Show)

	return &forbiddenTestEnv{router: r, owner: owner, other: other, admin: admin, linkID: link.ID}
}

// get issues a GET request as the given user (nil = anonymous), optionally
// marked as an HTMX request.
func (e *forbiddenTestEnv) get(t *testing.T, path string, user *store.User, htmx bool) *httptest.ResponseRecorder {
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

// assertStyled403 asserts a 403 status with the styled HTML page, not bare text.
func assertStyled403(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	if strings.TrimSpace(body) == "forbidden" {
		t.Fatal("body is bare-text \"forbidden\", want styled 403 page")
	}
	if !strings.Contains(body, "Access denied") {
		t.Errorf("body missing %q: %s", "Access denied", body)
	}
	if !strings.Contains(body, "/dashboard") {
		t.Error("body missing dashboard link")
	}
}

func TestForbidden_AdminRoute_FullPage(t *testing.T) {
	env := newForbiddenTestEnv(t)

	w := env.get(t, "/admin/keywords", env.other, false)
	assertStyled403(t, w)
	if !strings.Contains(w.Body.String(), "<html") {
		t.Error("full-page request should render the base layout (missing <html>)")
	}
}

func TestForbidden_AdminRoute_HTMXFragment(t *testing.T) {
	env := newForbiddenTestEnv(t)

	w := env.get(t, "/admin/keywords", env.other, true)
	assertStyled403(t, w)
	if strings.Contains(w.Body.String(), "<html") {
		t.Error("HTMX request should render only the content fragment, not the base layout")
	}
}

func TestForbidden_AdminRoute_AdminAllowed(t *testing.T) {
	env := newForbiddenTestEnv(t)

	w := env.get(t, "/admin/keywords", env.admin, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestForbidden_LinkDetail_NonOwner(t *testing.T) {
	env := newForbiddenTestEnv(t)

	w := env.get(t, "/dashboard/links/"+env.linkID, env.other, false)
	assertStyled403(t, w)
}

func TestForbidden_LinkDetail_OwnerAllowed(t *testing.T) {
	env := newForbiddenTestEnv(t)

	w := env.get(t, "/dashboard/links/"+env.linkID, env.owner, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestForbidden_LinkEdit_NonOwner(t *testing.T) {
	env := newForbiddenTestEnv(t)

	w := env.get(t, "/dashboard/links/"+env.linkID+"/edit", env.other, false)
	assertStyled403(t, w)
}

func TestForbidden_LinkStats_NonOwner(t *testing.T) {
	env := newForbiddenTestEnv(t)

	w := env.get(t, "/dashboard/links/"+env.linkID+"/stats", env.other, false)
	assertStyled403(t, w)
}

func TestForbidden_LinkStats_NonOwner_HTMXFragment(t *testing.T) {
	env := newForbiddenTestEnv(t)

	w := env.get(t, "/dashboard/links/"+env.linkID+"/stats", env.other, true)
	assertStyled403(t, w)
	if strings.Contains(w.Body.String(), "<html") {
		t.Error("HTMX request should render only the content fragment, not the base layout")
	}
}
