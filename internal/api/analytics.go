// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces", ADR-0021
package api

import (
	"net/http"
	"time"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

type analyticsAPIHandler struct {
	clicks *store.ClickStore
}

func newAnalyticsAPIHandler(cs *store.ClickStore) *analyticsAPIHandler {
	return &analyticsAPIHandler{clicks: cs}
}

// AnalyticsTopLink is one row in the top-links panel. trend_pct is the
// percentage change vs the previous equal-length period, and is null when the
// previous period count is zero (the "new" case) — never Infinity or a
// division error.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
type AnalyticsTopLink struct {
	LinkID        string   `json:"link_id"`
	Slug          string   `json:"slug"`
	Count         int64    `json:"count"`
	PreviousCount int64    `json:"previous_count"`
	TrendPct      *float64 `json:"trend_pct"`
}

// AnalyticsNeverClicked is one row in the never-clicked panel.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
type AnalyticsNeverClicked struct {
	LinkID    string    `json:"link_id"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

// AnalyticsReferrer is one row in the busiest-referrers panel.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
type AnalyticsReferrer struct {
	Host  string `json:"host"`
	Count int64  `json:"count"`
}

// AnalyticsResponse is the JSON shape for GET /api/v1/analytics. Aggregate
// counts only — no field anywhere in it names or identifies any clicker.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces"
type AnalyticsResponse struct {
	Period       string                  `json:"period"`
	Scope        string                  `json:"scope"`
	TopLinks     []AnalyticsTopLink      `json:"top_links"`
	NeverClicked []AnalyticsNeverClicked `json:"never_clicked"`
	TopReferrers []AnalyticsReferrer     `json:"top_referrers"`
}

// analyticsPeriodDays maps the period parameter to its window size: week
// (default) is the last 7 days, month the last 30.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
func analyticsPeriodDays(period string) (string, int, bool) {
	switch period {
	case "", "week":
		return "week", 7, true
	case "month":
		return "month", 30, true
	default:
		return "", 0, false
	}
}

// trendPct computes the percentage change vs the previous period, nil when
// the previous count is zero (the "new" marker case).
// Governing: SPEC-0021 REQ "Global Analytics Dashboard" — scenario "Trend
// against previous period"
func trendPct(count, previous int64) *float64 {
	if previous == 0 {
		return nil
	}
	pct := 100 * float64(count-previous) / float64(previous)
	return &pct
}

// GetAnalytics returns the global analytics dashboard panels.
// GET /api/v1/analytics
//
//	@Summary		Get global analytics
//	@Description	Aggregate analytics over the caller's personal scope — links they own or co-own plus links shared with them: the top 10 most-clicked links for the period with a trend vs the previous equal-length period (trend_pct is null when the previous period had zero clicks), links created at least 7 days ago with no recorded clicks (newest first, capped at 10), and the top referrer hosts. Counts only — no clicker identities. Admins may pass scope=all for instance-wide aggregates; scope=all from a non-admin is 403.
//	@Tags			Analytics
//	@Accept			json
//	@Produce		json
//	@Param			period	query	string	false	"Aggregation period: week (default, last 7 days) or month (last 30 days)"
//	@Param			scope	query	string	false	"Aggregation scope: mine (default) or all (admin only)"
//	@Success		200	{object}	AnalyticsResponse
//	@Failure		400	{object}	ErrorResponse
//	@Failure		401	{object}	ErrorResponse
//	@Failure		403	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerToken
//	@Router			/analytics [get]
//
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces", ADR-0021
func (h *analyticsAPIHandler) GetAnalytics(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	period, periodDays, ok := analyticsPeriodDays(r.URL.Query().Get("period"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid period parameter, expected week or month", CodeBadRequest)
		return
	}

	// scope=all is admin-only, per SPEC-0010's explicit admin-override style;
	// everyone — admins included — defaults to the personal scope.
	// Governing: SPEC-0021 REQ "Global Analytics Dashboard" — scenario "Admin toggle"
	scope := "mine"
	switch r.URL.Query().Get("scope") {
	case "", "mine":
	case "all":
		if !user.IsAdmin() {
			writeError(w, http.StatusForbidden, "forbidden", CodeForbidden)
			return
		}
		scope = "all"
	default:
		writeError(w, http.StatusBadRequest, "invalid scope parameter, expected mine or all", CodeBadRequest)
		return
	}

	// The scoped link-ID set is resolved in the store layer from link_owners
	// + link_shares, and every panel query is constrained to it — other
	// users' links, including their public links, contribute nothing.
	// Governing: SPEC-0021 REQ "Global Analytics Dashboard", Security
	// Requirements "Cross-User Aggregation Leakage"
	var scopeIDs []string
	if scope != "all" {
		var err error
		scopeIDs, err = h.clicks.AnalyticsScopeLinkIDs(r.Context(), user.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
			return
		}
	}

	g, err := h.clicks.GetGlobalAnalytics(r.Context(), scopeIDs, scope == "all", periodDays)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	resp := AnalyticsResponse{
		Period:       period,
		Scope:        scope,
		TopLinks:     make([]AnalyticsTopLink, 0, len(g.TopLinks)),
		NeverClicked: make([]AnalyticsNeverClicked, 0, len(g.NeverClicked)),
		TopReferrers: make([]AnalyticsReferrer, 0, len(g.TopReferrers)),
	}
	for _, tl := range g.TopLinks {
		resp.TopLinks = append(resp.TopLinks, AnalyticsTopLink{
			LinkID:        tl.LinkID,
			Slug:          tl.Slug,
			Count:         tl.Count,
			PreviousCount: tl.PreviousCount,
			TrendPct:      trendPct(tl.Count, tl.PreviousCount),
		})
	}
	for _, nc := range g.NeverClicked {
		resp.NeverClicked = append(resp.NeverClicked, AnalyticsNeverClicked{
			LinkID:    nc.LinkID,
			Slug:      nc.Slug,
			CreatedAt: nc.CreatedAt,
		})
	}
	for _, ref := range g.TopReferrers {
		resp.TopReferrers = append(resp.TopReferrers, AnalyticsReferrer{Host: ref.Name, Count: ref.Count})
	}

	writeJSON(w, http.StatusOK, resp)
}
