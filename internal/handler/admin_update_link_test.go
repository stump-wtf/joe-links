package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newAdminUpdateEnv wires an AdminHandler over an in-memory DB with one link.
func newAdminUpdateEnv(t *testing.T) (*store.LinkStore, *store.Link, http.Handler) {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	ks := store.NewKeywordStore(db)

	u, err := us.Upsert(context.Background(), "test", "sub1", "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	link, err := ls.Create(context.Background(), "target", "https://original.example.com", u.ID, "T", "D", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	h := NewAdminHandler(ls, us, ks)
	r := chi.NewRouter()
	r.Put("/admin/links/{id}", h.UpdateLink)
	return ls, link, r
}

// putForm submits an urlencoded PUT to the admin update endpoint.
func putForm(t *testing.T, router http.Handler, linkID string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/admin/links/"+linkID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Governing: SPEC-0009 REQ "Variable Placeholder Syntax" — the admin inline
// edit must validate the URL like the user-facing forms instead of writing
// whatever was submitted (issue #205).
func TestAdminUpdateLink_RejectsInvalidURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantMsg string
	}{
		{"empty URL", "", "URL is required."},
		{"duplicate variable placeholders", "https://x.example.com/$a/$a", "duplicate variable name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ls, link, router := newAdminUpdateEnv(t)

			rec := putForm(t, router, link.ID, url.Values{
				"url":   {tc.url},
				"title": {"New Title"},
			})

			body := rec.Body.String()
			if !strings.Contains(body, "alert-error") || !strings.Contains(body, tc.wantMsg) {
				t.Errorf("expected error toast containing %q; body=%s", tc.wantMsg, body)
			}
			// The row must stay in edit mode with the submitted values preserved.
			if !strings.Contains(body, "edit-link-"+link.ID) {
				t.Errorf("expected inline edit row to be re-rendered; body=%s", body)
			}
			// The link must be untouched.
			got, err := ls.GetByID(context.Background(), link.ID)
			if err != nil {
				t.Fatalf("reload link: %v", err)
			}
			if got.URL != "https://original.example.com" {
				t.Errorf("url = %q, want unchanged %q", got.URL, "https://original.example.com")
			}
			if got.Title != "T" {
				t.Errorf("title = %q, want unchanged %q", got.Title, "T")
			}
		})
	}
}

func TestAdminUpdateLink_ValidURLStillUpdates(t *testing.T) {
	ls, link, router := newAdminUpdateEnv(t)

	rec := putForm(t, router, link.ID, url.Values{
		"url":   {"https://updated.example.com"},
		"title": {"Updated"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "alert-error") {
		t.Errorf("unexpected error toast; body=%s", rec.Body.String())
	}
	got, err := ls.GetByID(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("reload link: %v", err)
	}
	if got.URL != "https://updated.example.com" {
		t.Errorf("url = %q, want %q", got.URL, "https://updated.example.com")
	}
}
