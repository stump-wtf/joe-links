// Governing: SPEC-0019 REQ "Suggest Endpoint", ADR-0019, ADR-0008, ADR-0009, ADR-0010
package api

import (
	"net/http"
	"strconv"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// LinkSuggestion is one autocomplete entry: a slug and its title (title may
// be empty).
// Governing: SPEC-0019 REQ "Suggest Endpoint"
type LinkSuggestion struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// SuggestLinksResponse is the body for GET /api/v1/links/suggest.
// Governing: SPEC-0019 REQ "Suggest Endpoint"
type SuggestLinksResponse struct {
	Suggestions []LinkSuggestion `json:"suggestions"`
}

// suggestLinksAPIHandler provides GET /api/v1/links/suggest — cheap
// autocomplete over existing links. Distinct from the POST operation on the
// same path (SPEC-0017 LLM metadata generation, suggest.go).
type suggestLinksAPIHandler struct {
	links *store.LinkStore
}

// Suggest returns ranked autocomplete suggestions for links visible to the
// caller.
// GET /api/v1/links/suggest
// Governing: SPEC-0019 REQ "Suggest Endpoint", ADR-0019
//
// @Summary      Autocomplete link suggestions
// @Description  Returns slug+title autocomplete suggestions for links visible to the caller (public, owned, co-owned, or shared; admins see all), ranked slug-prefix matches first, then slug-substring, then title/description matches. Expired and archived links are excluded. Distinct from POST /links/suggest, which generates LLM metadata for a new link.
// @Tags         Links
// @Produce      json
// @Param        q      query     string  false  "Query text, matched literally (LIKE wildcards are escaped); empty returns no suggestions; truncated to 64 characters"
// @Param        limit  query     int     false  "Max suggestions to return (default 5, max 10; higher values are clamped)"
// @Success      200  {object}  SuggestLinksResponse
// @Failure      400  {object}  ErrorResponse  "Non-numeric or non-positive limit"
// @Failure      401  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links/suggest [get]
func (h *suggestLinksAPIHandler) Suggest(w http.ResponseWriter, r *http.Request) {
	// Bearer-only auth is enforced by the router group middleware (SPEC-0006:
	// session cookies are never accepted on /api/v1); this nil check mirrors
	// every other authenticated handler.
	// Governing: SPEC-0019 REQ "Suggest Endpoint" scenario "Unauthenticated Request Rejected"
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	// Governing: SPEC-0019 REQ "Suggest Endpoint" — default 5, a limit above
	// 10 is clamped to 10, and a non-numeric limit is rejected with 400.
	// Non-positive numeric values are rejected the same way: the spec defines
	// no meaning for them and silently returning nothing would be misleading.
	limit := store.DefaultSuggestLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer", CodeBadRequest)
			return
		}
		if n > store.MaxSuggestLimit {
			n = store.MaxSuggestLimit
		}
		limit = n
	}

	// Lowercasing, 64-character truncation, wildcard escaping, and the
	// visibility + lifecycle filters all live in the store — the handler
	// never filters visibility itself (ADR-0019: one authorization code path).
	links, err := h.links.SuggestLinks(r.Context(), user.ID, user.Role == "admin", r.URL.Query().Get("q"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	// Governing: SPEC-0019 REQ "Suggest Endpoint" — the response is an object
	// with a "suggestions" array (always present, [] when empty).
	resp := SuggestLinksResponse{Suggestions: make([]LinkSuggestion, 0, len(links))}
	for _, l := range links {
		resp.Suggestions = append(resp.Suggestions, LinkSuggestion{Slug: l.Slug, Title: l.Title})
	}
	writeJSON(w, http.StatusOK, resp)
}
