package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexedwards/scs/v2"
)

// TestLogout_RedirectsToLandingPage guards the logout target: redirecting to
// /auth/login would immediately restart the OIDC flow, and with a live IdP
// session the user is silently signed back in — logout becomes a no-op
// (issue #205).
func TestLogout_RedirectsToLandingPage(t *testing.T) {
	sm := scs.New()
	h := NewHandlers(nil, sm, nil, "", nil, "", false)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/logout", h.Logout)
	srv := sm.LoadAndSave(mux)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want %q", loc, "/")
	}
}
