// Governing: SPEC-0008 REQ "Keyword Host Discovery", ADR-0011
package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ConfigResponse is the shape returned by GET /api/v1/config.
type ConfigResponse struct {
	// ShortKeyword is the short-link prefix the deployment advertises in its
	// UI (e.g. "go"). Either the JOE_SHORT_KEYWORD override or, when unset,
	// the first DNS label of the request host.
	ShortKeyword string `json:"short_keyword" example:"go"`
}

// configAPIHandler serves public extension-facing server configuration.
type configAPIHandler struct {
	shortKeyword string // optional override (JOE_SHORT_KEYWORD); "" = derive from Host
}

// registerConfigRoutes registers the public /config endpoint.
// No auth required — the browser extension calls this without a token so it
// can honor JOE_SHORT_KEYWORD deployments where the short prefix differs from
// the hostname's first label (e.g. "go/" on links.example.com).
// Governing: SPEC-0008 REQ "Keyword Host Discovery", ADR-0011
func registerConfigRoutes(r chi.Router, shortKeyword string) {
	h := &configAPIHandler{shortKeyword: shortKeyword}
	r.Get("/config", h.Get)
}

// Get returns public server configuration.
// GET /api/v1/config
// When no override is configured the first label of the request Host is
// returned, mirroring the web UI (internal/handler/templates.go newBasePage)
// so the API and UI always advertise the same short-link prefix.
//
// @Summary      Public server configuration
// @Description  Returns extension-facing configuration such as the short-link prefix (JOE_SHORT_KEYWORD override, else the first DNS label of the server hostname). No authentication required.
// @Tags         Config
// @Produce      json
// @Success      200  {object}  ConfigResponse
// @Router       /config [get]
func (h *configAPIHandler) Get(w http.ResponseWriter, r *http.Request) {
	kw := h.shortKeyword
	if kw == "" {
		host := r.Host
		if hostOnly, _, ok := strings.Cut(host, ":"); ok {
			host = hostOnly
		}
		kw = strings.SplitN(host, ".", 2)[0]
	}
	writeJSON(w, http.StatusOK, ConfigResponse{ShortKeyword: kw})
}
