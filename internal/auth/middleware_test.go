// Governing: SPEC-0001 REQ "Role-Based Access Control", ADR-0003
package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/store"
)

// requireRoleRequest runs a request through RequireRole("admin") with the
// given user pre-set on the request context (nil = no user).
func requireRoleRequest(t *testing.T, m *Middleware, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("admin content"))
	})
	h := m.RequireRole("admin")(next)

	req := httptest.NewRequest(http.MethodGet, "/admin/keywords", nil)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), UserContextKey, user))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRequireRole_NilForbidden_PlainText403(t *testing.T) {
	m := NewMiddleware(nil, nil)

	w := requireRoleRequest(t, m, &store.User{Role: "user"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "forbidden" {
		t.Errorf("body = %q, want plain-text %q fallback", got, "forbidden")
	}
}

func TestRequireRole_InjectedForbiddenHandler(t *testing.T) {
	m := NewMiddleware(nil, nil)
	called := false
	m.Forbidden = func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<h2>Access denied</h2>"))
	}

	w := requireRoleRequest(t, m, &store.User{Role: "user"})
	if !called {
		t.Fatal("injected Forbidden handler was not called")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if !strings.Contains(w.Body.String(), "Access denied") {
		t.Errorf("body = %q, want styled forbidden content", w.Body.String())
	}
}

func TestRequireRole_MissingUser_UsesInjectedForbidden(t *testing.T) {
	m := NewMiddleware(nil, nil)
	called := false
	m.Forbidden = func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusForbidden)
	}

	w := requireRoleRequest(t, m, nil)
	if !called {
		t.Fatal("injected Forbidden handler was not called for missing user")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestRequireRole_MatchingRole_PassesThrough(t *testing.T) {
	m := NewMiddleware(nil, nil)
	m.Forbidden = func(w http.ResponseWriter, r *http.Request) {
		t.Error("Forbidden handler must not be called for a matching role")
	}

	w := requireRoleRequest(t, m, &store.User{Role: "admin"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "admin content" {
		t.Errorf("body = %q, want next handler output", w.Body.String())
	}
}
