// Governing: SPEC-0001 REQ "Local Admin Authentication", ADR-0024
package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

const testAdminPassword = "correct horse battery staple"

func TestHashPassword_VerifiesAndDistinguishes(t *testing.T) {
	hash, err := HashPassword(testAdminPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, testAdminPassword) {
		t.Errorf("hash leaks the password: %q", hash)
	}
	if !strings.HasPrefix(hash, passwordHashAlgorithm+"$") {
		t.Errorf("hash %q is not labeled with %q", hash, passwordHashAlgorithm)
	}

	ok, err := VerifyPassword(testAdminPassword, hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword rejected the password it was built from")
	}

	ok, err = VerifyPassword("wrong password entirely", hash)
	if err != nil {
		t.Fatalf("VerifyPassword(wrong): %v", err)
	}
	if ok {
		t.Error("VerifyPassword accepted a wrong password")
	}

	// A second hash of the same password differs: the salt is fresh per call.
	other, err := HashPassword(testAdminPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if other == hash {
		t.Error("two hashes of one password are identical — no per-hash salt")
	}
}

func TestVerifyPassword_RejectsForeignFormats(t *testing.T) {
	for _, encoded := range []string{
		"",
		"bcrypt$10$abc",
		"pbkdf2-sha256$notanumber$abc$def",
		"pbkdf2-sha256$10$abc$def", // iteration count below the floor
		"pbkdf2-sha256$600000$!!!$def",
	} {
		if _, err := VerifyPassword("x", encoded); err == nil {
			t.Errorf("VerifyPassword(%q) accepted a hash this package did not write", encoded)
		}
	}
}

func TestEnsureLocalAdmin_CreatesAccountAndCredential(t *testing.T) {
	db := testutil.NewTestDB(t)
	users := store.NewUserStore(db)
	creds := store.NewLocalCredentialStore(db)
	ctx := context.Background()

	user, err := EnsureLocalAdmin(ctx, users, creds, "admin", "", testAdminPassword)
	if err != nil {
		t.Fatalf("EnsureLocalAdmin: %v", err)
	}
	if user.Provider != store.LocalProvider || user.Subject != "admin" {
		t.Errorf("account = (%q, %q), want (%q, %q)", user.Provider, user.Subject, store.LocalProvider, "admin")
	}
	if user.Role != "admin" {
		t.Errorf("role = %q, want admin", user.Role)
	}
	if user.Email != "admin@local" {
		t.Errorf("email = %q, want the local default admin@local", user.Email)
	}

	a := NewLocalAuthenticator(users, creds, "admin")
	signedIn, err := a.Authenticate(ctx, "admin", testAdminPassword)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if signedIn.ID != user.ID {
		t.Errorf("signed in as %s, want %s", signedIn.ID, user.ID)
	}

	// Restart with a rotated password: bootstrap overwrites the stored hash, so
	// the old one stops working rather than lingering in the database.
	rotated := "rotate this password now"
	if _, err := EnsureLocalAdmin(ctx, users, creds, "admin", "", rotated); err != nil {
		t.Fatalf("EnsureLocalAdmin(rotated): %v", err)
	}
	if _, err := a.Authenticate(ctx, "admin", testAdminPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("old password still works after rotation: %v", err)
	}
	if _, err := a.Authenticate(ctx, "admin", rotated); err != nil {
		t.Errorf("rotated password rejected: %v", err)
	}
}

func TestLocalAuthenticator_FailuresAreIndistinguishable(t *testing.T) {
	db := testutil.NewTestDB(t)
	users := store.NewUserStore(db)
	creds := store.NewLocalCredentialStore(db)
	ctx := context.Background()

	if _, err := EnsureLocalAdmin(ctx, users, creds, "admin", "admin@example.com", testAdminPassword); err != nil {
		t.Fatalf("EnsureLocalAdmin: %v", err)
	}
	a := NewLocalAuthenticator(users, creds, "admin")
	if a.Username() != "admin" {
		t.Errorf("Username() = %q, want admin", a.Username())
	}

	cases := []struct {
		name     string
		username string
		password string
	}{
		{"wrong password", "admin", "not the password"},
		{"unknown username", "someone-else", testAdminPassword},
		{"empty password", "admin", ""},
	}
	for _, tc := range cases {
		_, err := a.Authenticate(ctx, tc.username, tc.password)
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: err = %v, want ErrInvalidCredentials", tc.name, err)
		}
	}

	// Surrounding whitespace is what an operator pastes, not a different
	// username.
	if _, err := a.Authenticate(ctx, "  admin  ", testAdminPassword); err != nil {
		t.Errorf("padded username rejected: %v", err)
	}
}

func TestLocalAuthenticator_RespectsAdminRoleGrant(t *testing.T) {
	db := testutil.NewTestDB(t)
	users := store.NewUserStore(db)
	creds := store.NewLocalCredentialStore(db)
	ctx := context.Background()

	// An account demoted through the admin UI is promoted again by startup,
	// matching the grant-only semantics of the OIDC admin settings.
	user, err := users.Upsert(ctx, store.LocalProvider, "admin", "admin@local", "admin", "user")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if user.Role != "user" {
		t.Fatalf("role = %q, want user", user.Role)
	}
	if _, err := EnsureLocalAdmin(ctx, users, creds, "admin", "admin@local", testAdminPassword); err != nil {
		t.Fatalf("EnsureLocalAdmin: %v", err)
	}
	after, err := users.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if after.Role != "admin" {
		t.Errorf("role = %q after bootstrap, want admin", after.Role)
	}
}

func TestEnsureLocalAdmin_RejectsIncompleteConfig(t *testing.T) {
	db := testutil.NewTestDB(t)
	users := store.NewUserStore(db)
	creds := store.NewLocalCredentialStore(db)

	for _, tc := range []struct{ username, password string }{{"", "pw"}, {"admin", ""}} {
		if _, err := EnsureLocalAdmin(context.Background(), users, creds, tc.username, "", tc.password); err == nil {
			t.Errorf("EnsureLocalAdmin(%q, %q) succeeded, want an error", tc.username, tc.password)
		}
	}
}

func TestLoginLimiter_BlocksAfterMaxFailures(t *testing.T) {
	l := newLoginLimiter(3, 5*time.Minute)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("203.0.113.9", now); !ok {
			t.Fatalf("attempt %d blocked before the limit", i+1)
		}
		l.record("203.0.113.9", now)
	}
	ok, wait := l.allow("203.0.113.9", now)
	if ok {
		t.Fatal("attempts allowed past the limit")
	}
	if wait <= 0 || wait > 5*time.Minute {
		t.Errorf("Retry-After = %v, want within the window", wait)
	}

	// Another address keeps its own budget.
	if ok, _ := l.allow("203.0.113.10", now); !ok {
		t.Error("one address's failures blocked another address")
	}

	// The window slides: once the oldest failure ages out, one attempt is
	// allowed again.
	if ok, _ := l.allow("203.0.113.9", now.Add(5*time.Minute)); !ok {
		t.Error("still blocked after the whole window passed")
	}
}

func TestLoginLimiter_SuccessClearsFailures(t *testing.T) {
	l := newLoginLimiter(2, 5*time.Minute)
	now := time.Now()
	l.record("host", now)
	l.reset("host")
	if _, ok := l.failures["host"]; ok {
		t.Error("reset left the address tracked")
	}
	if ok, _ := l.allow("host", now); !ok {
		t.Error("address still blocked after a successful sign-in")
	}
}

func TestLoginLimiter_PrunesExpiredAddresses(t *testing.T) {
	l := newLoginLimiter(2, time.Minute)
	now := time.Now()
	for i := 0; i < maxTrackedLoginKeys+5; i++ {
		l.record("addr-"+strconv.Itoa(i), now)
	}
	l.record("latest", now.Add(2*time.Minute))
	if len(l.failures) > 1 {
		t.Errorf("tracker holds %d addresses after pruning expired ones, want only the live one", len(l.failures))
	}
}

func TestLoginAttemptKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	req.RemoteAddr = "198.51.100.7:54321"
	if got := loginAttemptKey(req); got != "198.51.100.7" {
		t.Errorf("loginAttemptKey = %q, want the peer address without its port", got)
	}

	req.RemoteAddr = "no-port-here"
	if got := loginAttemptKey(req); got != "no-port-here" {
		t.Errorf("loginAttemptKey = %q, want RemoteAddr unchanged when it has no port", got)
	}
}
