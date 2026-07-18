// Governing: SPEC-0016 REQ "Link Stats Dashboard Page", ADR-0016
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
package handler

import (
	"log"
	"net/http"
	"net/url"
	"strconv"

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
	// ShowClickers gates the per-click user column: attribution is
	// manager-only because authenticated clickers on a secure link proxy the
	// hidden share roster. See PR #255 security review.
	ShowClickers bool
	// ChartData feeds the stats_chart partial: the daily-clicks SVG chart
	// with its 30/90-day window toggle. Counts only — no per-user data.
	// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
	ChartData StatsChartData
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

	// Daily time series chart over a selectable 30 (default) / 90-day window.
	// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
	chart, err := h.loadChart(r, link.ID, chartDays(r))
	if err != nil {
		http.Error(w, "could not load click series", http.StatusInternalServerError)
		return
	}

	data := StatsPage{
		BasePage:     newBasePage(r, user),
		User:         user,
		Link:         link,
		Stats:        stats,
		RecentClicks: recent,
		ShowClickers: caps.CanManageShares,
		ChartData:    chart,
	}

	if isHTMX(r) {
		renderPageFragment(w, "links/stats.html", "content", data)
		return
	}
	render(w, "links/stats.html", data)
}

// Chart serves the daily-clicks chart partial. The 30/90-day window toggle on
// the stats page targets this route with hx-get so activating a toggle swaps
// only the chart fragment; non-HTMX requests are redirected to the enclosing
// stats page with the same window so the URL stays shareable.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
func (h *StatsHandler) Chart(w http.ResponseWriter, r *http.Request) {
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

	// Same CanStats gate as the enclosing stats page: owners, co-owners,
	// admins, and share recipients. The chart displays counts only and
	// carries no per-user information.
	// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", REQ "Capability Gating of Analytics Surfaces"
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		log.Printf("stats chart: capability check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !caps.CanStats {
		RenderForbidden(w, r)
		return
	}

	days := chartDays(r)

	if !isHTMX(r) {
		http.Redirect(w, r, "/dashboard/links/"+link.ID+"/stats?days="+strconv.Itoa(days), http.StatusFound)
		return
	}

	chart, err := h.loadChart(r, link.ID, days)
	if err != nil {
		http.Error(w, "could not load click series", http.StatusInternalServerError)
		return
	}
	renderFragment(w, "stats_chart", chart)
}

// chartDays parses the days window for the web chart. The API rejects
// non-30/90 values with 400 (SPEC-0021 REQ "Time Series API"); the web UI
// falls back to 30.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func chartDays(r *http.Request) int {
	if r.URL.Query().Get("days") == "90" {
		return 90
	}
	return 30
}

// loadChart fetches the gap-filled daily series and builds the SVG view model.
//
// Retention (JOE_CLICK_RETENTION) is wired by the retention story (#278);
// until then the web chart computes with retention disabled (retentionDays 0),
// matching the API. The Pruned rendering path — no-data bands distinct from
// zero-count days, lower-bound bars on the horizon-crossing bucket — is fully
// implemented and template-tested, so #278 only threads configuration here.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
func (h *StatsHandler) loadChart(r *http.Request, linkID string, days int) (StatsChartData, error) {
	series, err := h.clicks.GetDailyClickSeries(r.Context(), linkID, days, 0)
	if err != nil {
		return StatsChartData{}, err
	}
	return StatsChartData{LinkID: linkID, Days: days, Chart: buildClickChart(series, days)}, nil
}
