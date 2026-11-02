// Governing: SPEC-0001 REQ "Local Admin Authentication", ADR-0024
package handler

import (
	"net/http"

	"github.com/joestump/joe-links/internal/auth"
)

// LoginHandler renders the sign-in form for local mode, the deployment in which
// joe-links has no OIDC provider and signs in its configured administrator with
// a username and password (ADR-0024).
type LoginHandler struct{}

// NewLoginHandler creates a new LoginHandler.
func NewLoginHandler() *LoginHandler { return &LoginHandler{} }

// Render draws the form. failure is the message to show above it ("" for a
// fresh form), and returnURL is the already-sanitized local path the form posts
// back with, so the redirect after a successful sign-in cannot leave this host.
func (h *LoginHandler) Render(w http.ResponseWriter, r *http.Request, returnURL, failure string) {
	render(w, "login.html", loginPage{
		BasePage:  newBasePage(r, nil),
		ReturnURL: returnURL,
		Failure:   failure,
	})
}

type loginPage struct {
	BasePage
	ReturnURL string
	Failure   string
}

var _ auth.LoginPageFunc = (&LoginHandler{}).Render
