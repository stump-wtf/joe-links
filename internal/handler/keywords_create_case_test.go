// Tests for SPEC-0019 REQ "Case-Folded Keyword Matching" (issue #284), ADR-0027
// Confirmation 4: keyword intake still rejects uppercase names, so the stored
// corpus stays canonical lowercase and folding the lookup candidate in
// GetByKeyword is exact.
// Governing: SPEC-0019 REQ "Case-Folded Keyword Matching", SPEC-0011 REQ "Admin Keywords Screen", ADR-0027
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

// newKeywordsEnv wires a KeywordsHandler over an in-memory DB.
func newKeywordsEnv(t *testing.T) (*store.KeywordStore, http.Handler) {
	t.Helper()
	db := testutil.NewTestDB(t)
	ks := store.NewKeywordStore(db)
	r := chi.NewRouter()
	r.Post("/admin/keywords", NewKeywordsHandler(ks).Create)
	return ks, r
}

// Scenario: Uppercase Keyword Still Rejected At Creation
// Governing: SPEC-0019 REQ "Case-Folded Keyword Matching"
func TestAdminCreateKeyword_RejectsUppercase(t *testing.T) {
	for _, name := range []string{"GH", "Gh", "gH"} {
		t.Run(name, func(t *testing.T) {
			ks, router := newKeywordsEnv(t)

			form := url.Values{
				"keyword":      {name},
				"url_template": {"https://github.com/{slug}"},
			}
			req := httptest.NewRequest(http.MethodPost, "/admin/keywords", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if !strings.Contains(rec.Body.String(), "Keyword must be lowercase letters, digits, and hyphens") {
				t.Errorf("keyword %q: expected lowercase-only validation error; body=%s", name, rec.Body.String())
			}

			keywords, err := ks.List(context.Background())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(keywords) != 0 {
				t.Errorf("keyword %q: corpus = %d keywords, want 0", name, len(keywords))
			}
		})
	}
}
