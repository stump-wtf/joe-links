// Governing: SPEC-0016 REQ "Link Stats Dashboard Page", ADR-0016
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", REQ "Click Breakdowns", REQ "CSV Export", ADR-0021
package handler

import (
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

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
	// Breakdowns feeds the stats_breakdowns partial: referrer/browser/OS/auth
	// tables below the chart, over the same window. Counts only.
	// Governing: SPEC-0021 REQ "Click Breakdowns", ADR-0021
	Breakdowns StatsBreakdownsData
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
	days := chartDays(r)
	chart, err := h.loadChart(r, link.ID, days)
	if err != nil {
		http.Error(w, "could not load click series", http.StatusInternalServerError)
		return
	}

	// Breakdown tables below the chart, over the same window. Counts only —
	// every CanStats viewer, share recipients included, gets them in full.
	// Governing: SPEC-0021 REQ "Click Breakdowns", ADR-0021
	breakdowns, err := h.loadBreakdowns(r, link.ID, days, false)
	if err != nil {
		http.Error(w, "could not load click breakdowns", http.StatusInternalServerError)
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
		Breakdowns:   breakdowns,
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

	// The breakdown tables cover the same selectable window as the chart, so
	// the window toggle updates both: the chart is the primary swap target and
	// the breakdowns ride along as an hx-swap-oob fragment.
	// Governing: SPEC-0021 REQ "Click Breakdowns" — same 30/90-day window as the series
	breakdowns, err := h.loadBreakdowns(r, link.ID, days, true)
	if err != nil {
		http.Error(w, "could not load click breakdowns", http.StatusInternalServerError)
		return
	}

	renderFragment(w, "stats_chart", chart)
	renderFragment(w, "stats_breakdowns", breakdowns)
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

// loadBreakdowns fetches the counts-only breakdown tables and builds the
// template payload; oob marks the render that accompanies the chart
// window-toggle fragment.
// Governing: SPEC-0021 REQ "Click Breakdowns", ADR-0021
func (h *StatsHandler) loadBreakdowns(r *http.Request, linkID string, days int, oob bool) (StatsBreakdownsData, error) {
	b, err := h.clicks.GetClickBreakdowns(r.Context(), linkID, days)
	if err != nil {
		return StatsBreakdownsData{}, err
	}
	return newStatsBreakdownsData(linkID, days, oob, b), nil
}

// exportRowCap is the per-response export row cap, a var so tests can lower
// it; production always uses store.ExportRowCap.
// Governing: SPEC-0021 REQ "CSV Export"
var exportRowCap = store.ExportRowCap

// Export streams the link's click history as CSV — the session-authenticated
// twin of GET /api/v1/links/{id}/stats/export, backing the "Export CSV"
// button on the stats page (SPEC-0006 forbids sessions on /api/v1, so the UI
// cannot call the API route). Both routes share one store-level streaming
// iterator and one CSV encoder, so their output is identical for identical
// inputs (ADR-0021).
// Governing: SPEC-0021 REQ "CSV Export", ADR-0021
func (h *StatsHandler) Export(w http.ResponseWriter, r *http.Request) {
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

	// Export honors the exact same authorization as the stats page: CanStats
	// gates the rows; CanManageShares gates the user and raw user_agent
	// columns. A presented cursor is a keyset position, not a capability —
	// this check runs on every request regardless.
	// Governing: SPEC-0021 REQ "CSV Export", REQ "Capability Gating of Analytics Surfaces"
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		log.Printf("stats export: capability check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !caps.CanStats {
		RenderForbidden(w, r)
		return
	}

	// Optional from/to RFC 3339 window bounds and opaque continuation cursor;
	// invalid values are 400.
	// Governing: SPEC-0021 REQ "CSV Export"
	q := store.ClickExportQuery{LinkID: link.ID}
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, "invalid from parameter, expected RFC 3339 timestamp", http.StatusBadRequest)
			return
		}
		q.From = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, "invalid to parameter, expected RFC 3339 timestamp", http.StatusBadRequest)
			return
		}
		q.To = t
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		ts, cid, err := store.DecodeClickExportCursor(v)
		if err != nil {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		q.AfterTS, q.AfterID = ts, cid
	}

	// Response headers precede a streamed body, so truncation is determined
	// before streaming begins via a single keyset probe; the X-Next-Cursor
	// header's absence means the export is complete.
	// Governing: SPEC-0021 REQ "CSV Export"
	nextTS, nextID, truncated, err := h.clicks.ClickExportNextCursor(r.Context(), q, exportRowCap)
	if err != nil {
		http.Error(w, "could not prepare export", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+link.Slug+`-clicks.csv"`)
	if truncated {
		w.Header().Set("X-Next-Cursor", store.EncodeClickExportCursor(nextTS, nextID))
	}
	w.WriteHeader(http.StatusOK)

	if err := h.clicks.StreamClickExportCSV(r.Context(), w, q, exportRowCap, caps.CanManageShares); err != nil {
		// The status line and headers are already on the wire; all we can do
		// is stop and log — the truncated body signals the failure.
		log.Printf("stats export: streaming CSV for link %s failed: %v", link.ID, err)
	}
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
