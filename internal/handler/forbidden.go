// Governing: SPEC-0001 REQ "Role-Based Access Control", REQ "HTMX Hypermedia Interactions", ADR-0003
// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
// Governing: SPEC-0010 REQ "Secure Link Resolution"
package handler

import (
	"net/http"

	"github.com/joestump/joe-links/internal/auth"
)

// RenderForbidden writes a 403 response using the styled 403.html page instead
// of bare text. Full-page requests get the base layout (nav + theme survive);
// HTMX requests get just the "content" block so the fragment swaps cleanly.
// It is the shared forbidden renderer for all web-UI call sites, and is also
// injected into auth.Middleware (which cannot import this package) for
// RequireRole failures on /admin/* routes.
// Governing: SPEC-0001 REQ "Role-Based Access Control", ADR-0003
func RenderForbidden(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	data := notFoundPage{BasePage: newBasePage(r, user), User: user}
	if isHTMX(r) {
		renderPageFragment(w, "403.html", "content", data)
		return
	}
	render(w, "403.html", data)
}
