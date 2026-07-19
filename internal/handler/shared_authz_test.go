// Shared-link authorization across the web UI: share recipients get read-only
// access (list detail/stats actions, detail page, stats page) and styled 403s
// on every mutating route; owners keep full control.
//
// Governing: SPEC-0010 REQ "Link Shares Table", REQ "Dashboard Visibility Filtering"
// Governing: SPEC-0016 REQ "Link Stats Dashboard Page"
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

type sharedAuthzEnv struct {
	router    http.Handler
	owner     *store.User
	recipient *store.User
	stranger  *store.User
	admin     *store.User
	link      *store.Link
	clicks    *store.ClickStore
}

// newSharedAuthzEnv mirrors NewRouter's dashboard wiring with a secure link
// owned by owner and shared with recipient.
func newSharedAuthzEnv(t *testing.T) *sharedAuthzEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tagStore := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tagStore)
	ks := store.NewKeywordStore(db)
	us := store.NewUserStore(db)
	cs := store.NewClickStore(db)

	ctx := context.Background()
	owner, err := us.Upsert(ctx, "test", "sh-owner", "sh-owner@example.com", "Owner", "user")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	recipient, err := us.Upsert(ctx, "test", "sh-recipient", "sh-recipient@example.com", "Recipient", "user")
	if err != nil {
		t.Fatalf("seed recipient: %v", err)
	}
	stranger, err := us.Upsert(ctx, "test", "sh-stranger", "sh-stranger@example.com", "Stranger", "user")
	if err != nil {
		t.Fatalf("seed stranger: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "sh-admin", "sh-admin@example.com", "Admin", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	link, err := ls.Create(ctx, "shared-secret", "https://internal.example.com/doc", owner.ID, "Secret Doc", "", "secure")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	dashboard := NewDashboardHandler(ls, owns, tagStore, ks)
	links := NewLinksHandler(ls, owns, us, ks)
	statsHandler := NewStatsHandler(ls, cs, owns, 0)

	r := chi.NewRouter()
	r.Get("/dashboard", dashboard.Show)
	r.Get("/dashboard/links/{id}", links.Detail)
	r.Get("/dashboard/links/{id}/edit", links.Edit)
	r.Get("/dashboard/links/{id}/stats", statsHandler.Show)
	r.Get("/dashboard/links/{id}/confirm-delete", links.ConfirmDelete)
	r.Put("/dashboard/links/{id}", links.Update)
	r.Delete("/dashboard/links/{id}", links.Delete)
	r.Post("/dashboard/links/{id}/owners", links.AddOwner)
	r.Post("/dashboard/links/{id}/shares", links.AddShare)

	return &sharedAuthzEnv{router: r, owner: owner, recipient: recipient, stranger: stranger, admin: admin, link: link, clicks: cs}
}

// do issues a request as the given user (nil = anonymous).
func (e *sharedAuthzEnv) do(t *testing.T, method, path string, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// Governing: SPEC-0010 — "Shared with me" rows must not render dead Edit/Delete
// buttons for recipients, and must link to the read-only detail page.
func TestSharedList_RecipientGetsReadOnlyActions(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodGet, "/dashboard?filter=shared", env.recipient)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, env.link.Slug) {
		t.Fatalf("shared list missing shared link row; body=%s", body)
	}
	// Working read-only actions: detail link + stats link.
	if !strings.Contains(body, `href="/dashboard/links/`+env.link.ID+`"`) {
		t.Errorf("recipient row missing detail link /dashboard/links/%s", env.link.ID)
	}
	if !strings.Contains(body, "/dashboard/links/"+env.link.ID+"/stats") {
		t.Errorf("recipient row missing stats link")
	}
	// No dead mutating actions.
	if strings.Contains(body, "/dashboard/links/"+env.link.ID+"/edit") {
		t.Errorf("recipient row must not render Edit action")
	}
	if strings.Contains(body, "/dashboard/links/"+env.link.ID+"/confirm-delete") {
		t.Errorf("recipient row must not render Delete action")
	}
}

func TestDashboard_OwnerKeepsFullActions(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodGet, "/dashboard", env.owner)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, href := range []string{
		`href="/dashboard/links/` + env.link.ID + `"`,
		"/dashboard/links/" + env.link.ID + "/stats",
		"/dashboard/links/" + env.link.ID + "/edit",
		"/dashboard/links/" + env.link.ID + "/confirm-delete",
	} {
		if !strings.Contains(body, href) {
			t.Errorf("owner row missing %q", href)
		}
	}
}

// Governing: SPEC-0010 — recipients may view the detail page, read-only: no
// edit/delete controls, no owner management, no share roster.
func TestLinkDetail_RecipientReadOnly(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodGet, "/dashboard/links/"+env.link.ID, env.recipient)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, env.link.Slug) {
		t.Fatalf("detail page missing slug; body=%s", body)
	}
	if strings.Contains(body, "/dashboard/links/"+env.link.ID+"/edit") {
		t.Error("recipient detail must not render the Edit button")
	}
	if strings.Contains(body, "confirm-delete-modal") {
		t.Error("recipient detail must not render the Delete confirm modal")
	}
	if strings.Contains(body, "/dashboard/links/"+env.link.ID+"/owners") {
		t.Error("recipient detail must not render owner management controls")
	}
	if strings.Contains(body, "/dashboard/links/"+env.link.ID+"/shares") {
		t.Error("recipient detail must not render share management controls")
	}
	if strings.Contains(body, "Shared with") {
		t.Error("recipient detail must not reveal the share roster")
	}
}

func TestLinkDetail_OwnerKeepsControls(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodGet, "/dashboard/links/"+env.link.ID, env.owner)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"/dashboard/links/" + env.link.ID + "/edit",
		"confirm-delete-modal",
		"/dashboard/links/" + env.link.ID + "/owners",
		"/dashboard/links/" + env.link.ID + "/shares",
		"Shared with",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owner detail missing %q", want)
		}
	}
}

func TestLinkDetail_StrangerForbidden(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodGet, "/dashboard/links/"+env.link.ID, env.stranger)
	assertStyled403(t, w)
}

// Governing: SPEC-0016 — recipients may view the stats page read-only.
func TestLinkStats_RecipientAllowed(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodGet, "/dashboard/links/"+env.link.ID+"/stats", env.recipient)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestLinkStats_StrangerForbidden(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodGet, "/dashboard/links/"+env.link.ID+"/stats", env.stranger)
	assertStyled403(t, w)
}

// Governing: SPEC-0002 — mutations stay owner/admin-only; recipients get the
// styled 403 page (RenderForbidden), never a bare-text error.
func TestLinkMutations_RecipientStyled403(t *testing.T) {
	env := newSharedAuthzEnv(t)

	cases := []struct {
		name, method, path string
	}{
		{"edit form", http.MethodGet, "/dashboard/links/" + env.link.ID + "/edit"},
		{"update", http.MethodPut, "/dashboard/links/" + env.link.ID},
		{"confirm delete", http.MethodGet, "/dashboard/links/" + env.link.ID + "/confirm-delete"},
		{"delete", http.MethodDelete, "/dashboard/links/" + env.link.ID},
		{"add owner", http.MethodPost, "/dashboard/links/" + env.link.ID + "/owners"},
		{"add share", http.MethodPost, "/dashboard/links/" + env.link.ID + "/shares"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := env.do(t, tc.method, tc.path, env.recipient)
			assertStyled403(t, w)
		})
	}
}

func TestLinkDelete_OwnerAllowed(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodDelete, "/dashboard/links/"+env.link.ID, env.owner)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// Admins retain full control over shared links they do not own.
func TestLinkDetail_AdminKeepsControls(t *testing.T) {
	env := newSharedAuthzEnv(t)

	w := env.do(t, http.MethodGet, "/dashboard/links/"+env.link.ID, env.admin)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/dashboard/links/"+env.link.ID+"/edit") {
		t.Error("admin detail missing Edit button")
	}
}

// The web stats page hides the per-click user column from recipients:
// clicker attribution on a secure link proxies the hidden share roster.
// See PR #255 security review.
func TestLinkStats_RecipientSeesNoClickerAttribution(t *testing.T) {
	env := newSharedAuthzEnv(t)
	if err := env.clicks.RecordClick(context.Background(), store.ClickEvent{
		LinkID: env.link.ID, UserID: env.owner.ID, IPHash: "h1", UserAgent: "Test/1",
	}); err != nil {
		t.Fatalf("record click: %v", err)
	}

	ownerPage := env.do(t, http.MethodGet, "/dashboard/links/"+env.link.ID+"/stats", env.owner)
	if ownerPage.Code != http.StatusOK {
		t.Fatalf("owner stats status = %d, want 200", ownerPage.Code)
	}
	if !strings.Contains(ownerPage.Body.String(), "<th>User</th>") ||
		!strings.Contains(ownerPage.Body.String(), env.owner.DisplayName) {
		t.Errorf("owner stats page should show the clicker column and name")
	}

	recPage := env.do(t, http.MethodGet, "/dashboard/links/"+env.link.ID+"/stats", env.recipient)
	if recPage.Code != http.StatusOK {
		t.Fatalf("recipient stats status = %d, want 200", recPage.Code)
	}
	if strings.Contains(recPage.Body.String(), "<th>User</th>") {
		t.Errorf("recipient stats page must not render the clicker column")
	}
	if strings.Contains(recPage.Body.String(), ">"+env.owner.DisplayName+"<") {
		t.Errorf("recipient stats page must not render clicker display names")
	}
}
