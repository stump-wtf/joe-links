// Governing: SPEC-0001 REQ "Local Admin Authentication", "Authentication Mode Selection", ADR-0024
package auth

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/joestump/joe-links/internal/store"
)

const (
	// passwordHashAlgorithm labels the format stored in
	// local_credentials.password_hash, so a future scheme can be introduced
	// without guessing what existing rows mean.
	passwordHashAlgorithm = "pbkdf2-sha256"

	// passwordIterations and passwordKeyLength are the PBKDF2-HMAC-SHA256
	// parameters. A local sign-in is rare enough that a few hundred
	// milliseconds per attempt is the right trade against offline cracking, and
	// the login limiter below bounds how often an attacker can make the server
	// pay that cost.
	passwordIterations = 600_000
	passwordKeyLength  = 32
	passwordSaltLength = 16

	// minStorableIterations refuses to verify against a hash whose cost was
	// recorded absurdly low, which would mean a tampered row rather than a
	// parameter change made through this package.
	minStorableIterations = 100_000
)

// maxLoginAttempts and loginAttemptWindow bound credential guessing: failures
// are counted per client address, and past the count further attempts are
// refused until the oldest failure leaves the window. The budget is per address,
// not per username, so rotating usernames cannot multiply it.
const (
	maxLoginAttempts   = 10
	loginAttemptWindow = 5 * time.Minute
)

// ErrInvalidCredentials reports a failed sign-in. The message is the same
// whether the username, the password, or both were wrong.
var ErrInvalidCredentials = errors.New("invalid username or password")

// HashPassword derives a salted PBKDF2-HMAC-SHA256 digest and encodes it as
// pbkdf2-sha256$<iterations>$<salt>$<digest>, so verification carries its own
// parameters and outlives a parameter change.
func HashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyLength)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s",
		passwordHashAlgorithm,
		passwordIterations,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(digest),
	), nil
}

// VerifyPassword reports whether password matches encoded, re-deriving with the
// parameters recorded in encoded and comparing in constant time. A malformed
// hash is an error rather than a mismatch: it means the stored row is not
// something this package wrote.
func VerifyPassword(password, encoded string) (bool, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 4 || fields[0] != passwordHashAlgorithm {
		return false, errors.New("unrecognized password hash format")
	}
	iterations, err := strconv.Atoi(fields[1])
	if err != nil || iterations < minStorableIterations {
		return false, fmt.Errorf("stored password hash has an unusable iteration count %q", fields[1])
	}
	salt, err := base64.RawURLEncoding.DecodeString(fields[2])
	if err != nil || len(salt) < passwordSaltLength {
		return false, errors.New("stored password hash has an unusable salt")
	}
	wanted, err := base64.RawURLEncoding.DecodeString(fields[3])
	if err != nil || len(wanted) == 0 {
		return false, errors.New("stored password hash has an unusable digest")
	}
	digest, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(wanted))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(digest, wanted) == 1, nil
}

// LocalAuthenticator signs a user in with a username and password stored in the
// database. It is wired only in local mode: a deployment with an OIDC provider
// never constructs it, so no request path in that mode accepts a password.
type LocalAuthenticator struct {
	users    *store.UserStore
	creds    *store.LocalCredentialStore
	username string
	limiter  *loginLimiter
}

// NewLocalAuthenticator returns the authenticator for local mode. username is
// the single account local mode serves (config.Config.AdminUsername).
func NewLocalAuthenticator(users *store.UserStore, creds *store.LocalCredentialStore, username string) *LocalAuthenticator {
	return &LocalAuthenticator{
		users:    users,
		creds:    creds,
		username: username,
		limiter:  newLoginLimiter(maxLoginAttempts, loginAttemptWindow),
	}
}

// Username is the account local mode authenticates.
func (a *LocalAuthenticator) Username() string { return a.username }

// Authenticate returns the local account when username and password match its
// stored hash. Any mismatch reports ErrInvalidCredentials, including an unknown
// username, which costs a fixed dummy derivation so that a missing account and a
// wrong password take the same time.
func (a *LocalAuthenticator) Authenticate(ctx context.Context, username, password string) (*store.User, error) {
	username = strings.TrimSpace(username)
	known := subtle.ConstantTimeCompare([]byte(username), []byte(a.username)) == 1
	if !known || password == "" {
		// Same cost as a real verification, so timing does not disclose which
		// usernames exist.
		if _, err := HashPassword(password); err != nil {
			return nil, err
		}
		return nil, ErrInvalidCredentials
	}
	cred, err := a.creds.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	ok, err := VerifyPassword(password, cred.PasswordHash)
	if err != nil {
		return nil, fmt.Errorf("verify local password for %q: %w", username, err)
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}
	return cred.User, nil
}

// AllowAttempt reports whether this client address may try to sign in now, and
// how long it must wait when it may not.
func (a *LocalAuthenticator) AllowAttempt(r *http.Request) (bool, time.Duration) {
	return a.limiter.allow(loginAttemptKey(r), time.Now())
}

// RecordFailedAttempt counts a failed sign-in for this client address.
func (a *LocalAuthenticator) RecordFailedAttempt(r *http.Request) {
	a.limiter.record(loginAttemptKey(r), time.Now())
}

// RecordSuccessfulAttempt clears the failure count for this client address, so
// an operator who mistyped twice is not throttled after signing in.
func (a *LocalAuthenticator) RecordSuccessfulAttempt(r *http.Request) {
	a.limiter.reset(loginAttemptKey(r))
}

// loginAttemptKey is the limiter's key: the client address the router resolved
// (ADR-0023), falling back to the address that opened the connection. It never
// reads a header itself, so a caller cannot pick which bucket it lands in.
func loginAttemptKey(r *http.Request) string {
	if ip := middleware.GetClientIP(r.Context()); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// loginLimiter counts failed sign-ins per client address inside a sliding
// window. Entries exist only while they can still block someone, and are dropped
// once they cannot, so a scan across many addresses cannot grow it without
// bound.
type loginLimiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	failures map[string][]time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, failures: map[string][]time.Time{}}
}

func (l *loginLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	stamps := l.retain(key, now)
	if len(stamps) < l.max {
		return true, 0
	}
	return false, l.window - now.Sub(stamps[0])
}

func (l *loginLimiter) record(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) > maxTrackedLoginKeys {
		l.prune(now)
	}
	l.failures[key] = append(l.retain(key, now), now)
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// retain returns the stamps for key that are still inside the window, dropping
// the entry once none are left. Caller holds l.mu.
func (l *loginLimiter) retain(key string, now time.Time) []time.Time {
	stamps := l.failures[key]
	kept := stamps[:0]
	for _, t := range stamps {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, key)
		return nil
	}
	l.failures[key] = kept
	return kept
}

// prune drops every expired entry; it runs only when the tracker has grown past
// its ceiling, which a scan across a wide address range would otherwise do.
func (l *loginLimiter) prune(now time.Time) {
	for key := range l.failures {
		l.retain(key, now)
	}
}

// maxTrackedLoginKeys bounds how many client addresses carry a failure count at
// once before pruning is forced.
const maxTrackedLoginKeys = 10_000

// EnsureLocalAdmin creates or refreshes the local-mode administrator: the users
// row (provider 'local', subject = username, role admin) and a password hash
// derived from password. Startup calls it on every boot, so rotating
// JOE_ADMIN_PASSWORD takes effect on the next restart and a hash written by an
// older run cannot outlive the env that set it.
//
// The role is granted, never revoked: like the OIDC admin grants
// (JOE_ADMIN_EMAIL, JOE_OIDC_ADMIN_GROUPS) it can promote, and demotion happens
// through the admin UI. Governing: SPEC-0001 REQ "Local User Records".
func EnsureLocalAdmin(ctx context.Context, users *store.UserStore, creds *store.LocalCredentialStore, username, email, password string) (*store.User, error) {
	if username == "" || password == "" {
		return nil, errors.New("local admin bootstrap needs both a username and a password")
	}
	if email == "" {
		email = username + "@local"
	}
	user, err := users.Upsert(ctx, store.LocalProvider, username, email, username, "admin")
	if err != nil {
		return nil, fmt.Errorf("bootstrap local admin: %w", err)
	}
	if user.Role != "admin" {
		user, err = users.UpdateRole(ctx, user.ID, "admin")
		if err != nil {
			return nil, fmt.Errorf("grant admin role to local account: %w", err)
		}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash local admin password: %w", err)
	}
	if err := creds.SetPasswordHash(ctx, user.ID, hash); err != nil {
		return nil, fmt.Errorf("store local admin credential: %w", err)
	}
	return user, nil
}
