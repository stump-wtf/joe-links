// Governing: SPEC-0016 REQ "Link Stats Dashboard Page", ADR-0016
package handler

import (
	"log"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// StatsPage is the template data for the link analytics view.
// Governing: SPEC-0016 REQ "Link Stats Dashboard Page", ADR-0016
type StatsPage struct {
	BasePage
	User         *store.User
	Link         *store.Link
	Stats        store.ClickStats
	RecentClicks []store.RecentClick
}

// StatsHandler serves the per-link analytics page.
type StatsHandler struct {
	links  *store.LinkStore
	clicks *store.ClickStore
	owns   *store.OwnershipStore
}

// NewStatsHandler creates a new StatsHandler.
func NewStatsHandler(ls *store.LinkStore, cs *store.ClickStore, os *store.OwnershipStore) *StatsHandler {
	return &StatsHandler{links: ls, clicks: cs, owns: os}
}

// Show renders the stats page for a single link.
// Governing: SPEC-0016 REQ "Link Stats Dashboard Page", ADR-0016
func (h *StatsHandler) Show(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/auth/login?return_url="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}

	id := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), id)
	if err != nil {
		// Governing: SPEC-0016 REQ "Link Stats Dashboard Page" — styled 404, not bare text
		w.WriteHeader(http.StatusNotFound)
		data := notFoundPage{BasePage: newBasePage(r, user), User: user}
		if isHTMX(r) {
			renderPageFragment(w, "404.html", "content", data)
			return
		}
		render(w, "404.html", data)
		return
	}

	// Owners/co-owners/admins and share recipients may view stats (read-only).
	// Governing: SPEC-0016 REQ "Link Stats Dashboard Page"
	// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		log.Printf("stats: capability check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !caps.CanStats {
		// Governing: SPEC-0016 REQ "Link Stats Dashboard Page" — styled 403, not bare text
		RenderForbidden(w, r)
		return
	}

	stats, err := h.clicks.GetClickStats(r.Context(), link.ID)
	if err != nil {
		http.Error(w, "could not load stats", http.StatusInternalServerError)
		return
	}

	recent, err := h.clicks.ListRecentClicks(r.Context(), link.ID, 50)
	if err != nil {
		http.Error(w, "could not load recent clicks", http.StatusInternalServerError)
		return
	}

	data := StatsPage{
		BasePage:     newBasePage(r, user),
		User:         user,
		Link:         link,
		Stats:        stats,
		RecentClicks: recent,
	}

	if isHTMX(r) {
		renderPageFragment(w, "links/stats.html", "content", data)
		return
	}
	render(w, "links/stats.html", data)
}
