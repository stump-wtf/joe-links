// Governing: SPEC-0001 REQ "Local Admin Authentication", "Authentication Mode Selection", ADR-0024
package handler

// These exercise the real NewRouter so the local-mode login page, its form
// fields, and the POST route are covered as a deployed instance wires them.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

const loginTestPassword = "a-sufficiently-long-secret"

// newLoginTestRouter builds the production router in local mode, with the admin
// account bootstrapped the way serve does at startup.
func newLoginTestRouter(t *testing.T, localMode bool) http.Handler {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	creds := store.NewLocalCredentialStore(db)

	sm := scs.New()
	h := auth.NewHandlers(nil, sm, us, "", nil, "groups", false)
	if localMode {
		if _, err := auth.EnsureLocalAdmin(context.Background(), us, creds, "admin", "admin@example.com", loginTestPassword); err != nil {
			t.Fatalf("EnsureLocalAdmin: %v", err)
		}
		h.Local = auth.NewLocalAuthenticator(us, creds, "admin")
	}

	return NewRouter(Deps{
		SessionManager: sm,
		AuthHandlers:   h,
		AuthMiddleware: auth.NewMiddleware(sm, us),
		LinkStore:      ls,
		OwnershipStore: owns,
		TagStore:       tags,
		UserStore:      us,
		TokenStore:     auth.NewSQLTokenStore(db),
		KeywordStore:   store.NewKeywordStore(db),
		ClickStore:     store.NewClickStore(db),
		ClickCh:        make(chan store.ClickEvent, 8),
	})
}

func TestRouter_LocalModeServesLoginForm(t *testing.T) {
	router := newLoginTestRouter(t, true)

	req := httptest.NewRequest(http.MethodGet, "/auth/login?return_url=/links/new", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /auth/login status = %d, want 200 (body: %s)", w.Code, firstBytes(w.Body.String()))
	}
	body := w.Body.String()
	for _, want := range []string{`action="/auth/login"`, `name="username"`, `name="password"`, `name="return_url" value="/links/new"`} {
		if !strings.Contains(body, want) {
			t.Errorf("login form is missing %s", want)
		}
	}
	if strings.Contains(body, "password\" value=") {
		t.Error("login form pre-fills the password field")
	}
}

func TestRouter_LocalModeSignInStartsSession(t *testing.T) {
	router := newLoginTestRouter(t, true)

	form := url.Values{"username": {"admin"}, "password": {loginTestPassword}, "return_url": {"/dashboard"}}
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("POST /auth/login status = %d, want 302 (body: %s)", w.Code, firstBytes(w.Body.String()))
	}
	if got := w.Header().Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q, want /dashboard", got)
	}
	if len(w.Result().Cookies()) == 0 {
		t.Error("sign-in set no session cookie")
	}
}

func TestRouter_LocalModeRejectsBadPassword(t *testing.T) {
	router := newLoginTestRouter(t, true)

	form := url.Values{"username": {"admin"}, "password": {"guessing"}, "return_url": {"/dashboard"}}
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid username or password") {
		t.Errorf("body does not report the failure: %s", firstBytes(w.Body.String()))
	}
}

// In OIDC mode the POST route exists but refuses: an instance with a provider
// must not answer a password form.
func TestRouter_OIDCModeRefusesPasswordSignIn(t *testing.T) {
	router := newLoginTestRouter(t, false)

	form := url.Values{"username": {"admin"}, "password": {"whatever"}}
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func firstBytes(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
