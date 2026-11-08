// Governing: SPEC-0001 REQ "Local Admin Authentication", "Authentication Mode Selection", ADR-0024
package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// localLoginFixture wires Handlers in local mode over a fresh database, with a
// stub login page that records what it was asked to render.
func localLoginFixture(t *testing.T, password string) (*Handlers, *chi.Mux, *int) {
	t.Helper()

	db := testutil.NewTestDB(t)
	users := store.NewUserStore(db)
	creds := store.NewLocalCredentialStore(db)
	if _, err := EnsureLocalAdmin(context.Background(), users, creds, "admin", "admin@example.com", password); err != nil {
		t.Fatalf("EnsureLocalAdmin: %v", err)
	}

	sm := NewSessionManager(db, "sqlite3", time.Hour, false)
	h := NewHandlers(nil, sm, users, "", nil, "", false)
	h.Local = NewLocalAuthenticator(users, creds, "admin")

	rendered := 0
	h.LoginPage = func(w http.ResponseWriter, r *http.Request, returnURL, failure string) {
		rendered++
		if failure != "" {
			_, _ = w.Write([]byte("FAILURE:" + failure))
		}
		_, _ = w.Write([]byte("FORM return_url=" + returnURL))
	}

	mw := NewMiddleware(sm, users)
	dashboard := mw.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("dashboard"))
	}))

	r := chi.NewRouter()
	r.Use(sm.LoadAndSave)
	r.Get("/auth/login", h.Login)
	r.Post("/auth/login", h.LocalLogin)
	r.Get("/dashboard", dashboard.ServeHTTP)

	return h, r, &rendered
}

func postLogin(t *testing.T, urlStr string, form url.Values, jar *cookiejar.Jar) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if jar != nil {
		client.Jar = jar
	}
	resp, err := client.PostForm(urlStr, form)
	if err != nil {
		t.Fatalf("POST %s: %v", urlStr, err)
	}
	return resp
}

func TestLocalLogin_RendersFormWithSanitizedReturnURL(t *testing.T) {
	_, router, rendered := localLoginFixture(t, testAdminPassword)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/auth/login?return_url=//evil.example.com/keys")
	if err != nil {
		t.Fatalf("GET /auth/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if *rendered != 1 {
		t.Errorf("login page rendered %d times, want 1", *rendered)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "FORM return_url=/dashboard") {
		t.Errorf("form carries an unsafe return_url: %q", body)
	}
}

func TestLocalLogin_ValidCredentialsStartSession(t *testing.T) {
	_, router, _ := localLoginFixture(t, testAdminPassword)
	srv := httptest.NewServer(router)
	defer srv.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.PostForm(srv.URL+"/auth/login", url.Values{
		"username":   {"admin"},
		"password":   {testAdminPassword},
		"return_url": {"/links/new"},
	})
	if err != nil {
		t.Fatalf("POST /auth/login: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/links/new" {
		t.Errorf("Location = %q, want the requested return_url", got)
	}

	client.CheckRedirect = nil
	dashboard, err := client.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	defer func() { _ = dashboard.Body.Close() }()
	if dashboard.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /dashboard status = %d, want 200 (session was not established)", dashboard.StatusCode)
	}
	if body := readAll(t, dashboard); body != "dashboard" {
		t.Errorf("body = %q", body)
	}
}

func TestLocalLogin_BadPasswordIsUniformFailure(t *testing.T) {
	for name, form := range map[string]url.Values{
		"wrong password": {"username": {"admin"}, "password": {"not the password"}},
		"unknown user":   {"username": {"root"}, "password": {testAdminPassword}},
	} {
		_, router, _ := localLoginFixture(t, testAdminPassword)
		srv := httptest.NewServer(router)

		resp := postLogin(t, srv.URL+"/auth/login", form, nil)
		body := readAll(t, resp)
		_ = resp.Body.Close()
		srv.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
		if !strings.Contains(body, "FAILURE:Invalid username or password.") {
			t.Errorf("%s: body = %q, want the same message for every failure", name, body)
		}
		if strings.Contains(body, "root") {
			t.Errorf("%s: body leaks the submitted username: %q", name, body)
		}
	}
}

func TestLocalLogin_BlocksGuessingAfterMaxFailures(t *testing.T) {
	_, router, _ := localLoginFixture(t, testAdminPassword)
	srv := httptest.NewServer(router)
	defer srv.Close()

	form := url.Values{"username": {"admin"}, "password": {"guess"}}
	var last *http.Response
	for i := 0; i < maxLoginAttempts; i++ {
		last = postLogin(t, srv.URL+"/auth/login", form, nil)
		last.Body.Close()
	}
	if last.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after %d failures = %d, want 401", maxLoginAttempts, last.StatusCode)
	}

	blocked := postLogin(t, srv.URL+"/auth/login", url.Values{
		"username": {"admin"},
		"password": {testAdminPassword},
	}, nil)
	defer blocked.Body.Close()
	if blocked.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the failure budget is spent", blocked.StatusCode)
	}
	if blocked.Header.Get("Retry-After") == "" {
		t.Error("429 without a Retry-After header")
	}
}

func TestLocalLogin_OIDCModeRefusesPasswordSignIn(t *testing.T) {
	db := testutil.NewTestDB(t)
	sm := NewSessionManager(db, "sqlite3", time.Hour, false)
	h := NewHandlers(nil, sm, store.NewUserStore(db), "", nil, "", false)

	r := chi.NewRouter()
	r.Use(sm.LoadAndSave)
	r.Post("/auth/login", h.LocalLogin)

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp := postLogin(t, srv.URL+"/auth/login", url.Values{"username": {"admin"}, "password": {"x"}}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: OIDC mode must not answer a password form", resp.StatusCode)
	}
}

func TestLocalLogin_RedirectIsAlwaysLocal(t *testing.T) {
	_, router, _ := localLoginFixture(t, testAdminPassword)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := postLogin(t, srv.URL+"/auth/login", url.Values{
		"username":   {"admin"},
		"password":   {testAdminPassword},
		"return_url": {"https://evil.example.com/steal"},
	}, nil)
	defer resp.Body.Close()

	if got := resp.Header.Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q, want the safe default /dashboard", got)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
