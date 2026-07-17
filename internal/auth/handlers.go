// Governing: SPEC-0001 REQ "OIDC-Only Authentication", ADR-0003
package auth

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/joestump/joe-links/internal/store"
)

const (
	cookieState        = "__auth_state"
	cookieCodeVerifier = "__auth_pkce"
	cookieRedirect     = "__auth_redirect"
)

// safeReturnURL returns u only if it is a safe local path (begins with a single
// "/" and is not protocol-relative), otherwise "/dashboard". This prevents an
// open redirect via ?return_url=https://evil.com or //evil.com after login.
// Governing: SPEC-0010 REQ "Secure Link Resolution"
func safeReturnURL(u string) string {
	if u == "" || u[0] != '/' || strings.HasPrefix(u, "//") || strings.HasPrefix(u, "/\\") {
		return "/dashboard"
	}
	return u
}

// Handlers provides HTTP handlers for the OIDC authentication flow.
type Handlers struct {
	provider      *Provider
	sessions      *scs.SessionManager
	users         *store.UserStore
	adminEmail    string
	adminGroups   []string // OIDC group names that grant the admin role
	groupsClaim   string   // OIDC claim name for groups (default: "groups")
	secureCookies bool
}

// NewHandlers creates a new Handlers with the given dependencies.
// Set secureCookies=false for local HTTP development.
func NewHandlers(p *Provider, sm *scs.SessionManager, us *store.UserStore, adminEmail string, adminGroups []string, groupsClaim string, secureCookies bool) *Handlers {
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	return &Handlers{
		provider:      p,
		sessions:      sm,
		users:         us,
		adminEmail:    adminEmail,
		adminGroups:   adminGroups,
		groupsClaim:   groupsClaim,
		secureCookies: secureCookies,
	}
}

// Login initiates the OIDC authorization code flow with PKCE.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	state, err := GenerateState()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Store state and verifier in short-lived cookies
	h.setPreAuthCookie(w, cookieState, state)
	h.setPreAuthCookie(w, cookieCodeVerifier, verifier)

	// Preserve the return URL.
	// Governing: SPEC-0010 REQ "Secure Link Resolution" — login flow reads return_url.
	// Sanitize to a local path to prevent an open redirect after authentication.
	h.setPreAuthCookie(w, cookieRedirect, safeReturnURL(r.URL.Query().Get("return_url")))

	http.Redirect(w, r, h.provider.AuthCodeURL(state, challenge), http.StatusFound)
}

// Callback handles the OIDC provider redirect after authentication.
func (h *Handlers) Callback(w http.ResponseWriter, r *http.Request) {
	// Validate state
	stateCookie, err := r.Cookie(cookieState)
	if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	// Get PKCE verifier
	verifierCookie, err := r.Cookie(cookieCodeVerifier)
	if err != nil {
		http.Error(w, "missing code verifier", http.StatusBadRequest)
		return
	}

	// Exchange code for tokens
	idToken, err := h.provider.Exchange(r.Context(), r.URL.Query().Get("code"), verifierCookie.Value)
	if err != nil {
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}

	// Extract claims — groups claim is dynamic based on config.
	var rawClaims map[string]interface{}
	if err := idToken.Claims(&rawClaims); err != nil {
		http.Error(w, "invalid claims", http.StatusUnauthorized)
		return
	}
	email, _ := rawClaims["email"].(string)
	name, _ := rawClaims["name"].(string)
	subject, _ := rawClaims["sub"].(string)

	// Determine the env-granted role from adminEmail and OIDC group membership.
	role := h.computeRole(email, rawClaims)

	// Upsert user record. role applies on INSERT only; for existing users the
	// stored role is preserved so manual promotions made through the admin UI
	// survive re-login, and env config can only promote (applyRoleGrant below).
	// Governing: SPEC-0001 REQ "Local User Records" — "On subsequent logins,
	// the stored role MUST be preserved."
	user, err := h.users.Upsert(r.Context(), idToken.Issuer, subject, email, name, role)
	if err != nil {
		log.Printf("auth callback: upsert user (issuer=%s subject=%s email=%s): %v", idToken.Issuer, subject, email, err)
		http.Error(w, "user record error", http.StatusInternalServerError)
		return
	}

	if promoted, err := h.applyRoleGrant(r.Context(), user, role); err != nil {
		// A failed promotion must not block login; the user keeps their
		// stored role for this session and the grant retries on next login.
		log.Printf("auth callback: promote user to admin (id=%s email=%s): %v", user.ID, user.Email, err)
	} else {
		user = promoted
	}

	// Create session
	if err := h.sessions.RenewToken(r.Context()); err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	h.sessions.Put(r.Context(), SessionUserIDKey, user.ID)
	h.sessions.Put(r.Context(), SessionRoleKey, user.Role)

	// Clear pre-auth cookies
	clearCookie(w, cookieState)
	clearCookie(w, cookieCodeVerifier)

	// Redirect
	redirectCookie, err := r.Cookie(cookieRedirect)
	redirect := "/dashboard"
	if err == nil && redirectCookie.Value != "" {
		redirect = safeReturnURL(redirectCookie.Value) // defense in depth
	}
	clearCookie(w, cookieRedirect)

	http.Redirect(w, r, redirect, http.StatusFound)
}

// computeRole returns the role granted by env-driven admin config: "admin"
// when email matches adminEmail (JOE_ADMIN_EMAIL) or the OIDC groups claim
// intersects adminGroups (JOE_OIDC_ADMIN_GROUPS), otherwise "user".
func (h *Handlers) computeRole(email string, rawClaims map[string]interface{}) string {
	if h.adminEmail != "" && email == h.adminEmail {
		return "admin"
	}
	if len(h.adminGroups) > 0 {
		if groups := rawClaims[h.groupsClaim]; groups != nil {
			var userGroups []string
			switch v := groups.(type) {
			case []interface{}:
				for _, g := range v {
					if s, ok := g.(string); ok {
						userGroups = append(userGroups, s)
					}
				}
			case []string:
				userGroups = v
			}
			adminSet := make(map[string]struct{}, len(h.adminGroups))
			for _, g := range h.adminGroups {
				adminSet[g] = struct{}{}
			}
			for _, g := range userGroups {
				if _, ok := adminSet[g]; ok {
					return "admin"
				}
			}
		}
	}
	return "user"
}

// applyRoleGrant promotes user to admin when the env-computed role grants it.
// Grant-only semantics: JOE_ADMIN_EMAIL / JOE_OIDC_ADMIN_GROUPS can promote a
// pre-existing user on any login but never demote, so manual promotions made
// through the admin UI survive subsequent logins. Demotion happens only via
// the admin UI/API (UserStore.UpdateRole).
// Governing: SPEC-0001 REQ "Local User Records" — "On subsequent logins, the
// stored role MUST be preserved."
func (h *Handlers) applyRoleGrant(ctx context.Context, user *store.User, computed string) (*store.User, error) {
	if computed != "admin" || user.Role == "admin" {
		return user, nil
	}
	promoted, err := h.users.UpdateRole(ctx, user.ID, "admin")
	if err != nil {
		return nil, err
	}
	log.Printf("auth callback: promoted user to admin via env-configured admin email/groups (id=%s email=%s)", user.ID, user.Email)
	return promoted, nil
}

// Logout destroys the session and redirects to the login page.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if err := h.sessions.Destroy(r.Context()); err != nil {
		http.Error(w, "logout error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/auth/login", http.StatusFound)
}

func (h *Handlers) setPreAuthCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   300, // 5 minutes
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:    name,
		Value:   "",
		Path:    "/",
		MaxAge:  -1,
		Expires: time.Unix(0, 0),
	})
}
