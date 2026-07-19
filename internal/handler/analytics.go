// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces", ADR-0021
package handler

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// AnalyticsTopLinkRow is one row of the top-links panel with its trend
// precomputed for the template: TrendLabel is "+50.0%" / "-12.5%" style, or
// empty when IsNew (previous period count zero — the "new" marker case).
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
type AnalyticsTopLinkRow struct {
	LinkID        string
	Slug          string
	Count         int64
	PreviousCount int64
	IsNew         bool
	TrendUp       bool
	TrendLabel    string
}

// AnalyticsPage is the template data for the global analytics dashboard.
// Aggregate counts only — no clicker names or identities anywhere.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces"
type AnalyticsPage struct {
	BasePage
	User         *store.User
	Period       string // "week" or "month"
	PeriodDays   int    // 7 or 30
	Scope        string // "mine" or "all"
	IsAdmin      bool   // gates the visible scope=all toggle
	TopLinks     []AnalyticsTopLinkRow
	NeverClicked []store.NeverClickedLink
	TopReferrers []store.BreakdownRow
	// RetentionDays relabels the never-clicked panel honestly: with retention
	// enabled, "never clicked" means "no clicks within retention".
	// Governing: SPEC-0021 REQ "Click Retention"
	RetentionDays int
}

// AnalyticsHandler serves the global analytics dashboard page.
type AnalyticsHandler struct {
	clicks *store.ClickStore
	// retention is the click-retention horizon in days (JOE_CLICK_RETENTION);
	// 0 = retention disabled.
	retention int
}

// NewAnalyticsHandler creates a new AnalyticsHandler.
func NewAnalyticsHandler(cs *store.ClickStore, retentionDays int) *AnalyticsHandler {
	return &AnalyticsHandler{clicks: cs, retention: retentionDays}
}

// analyticsPeriod parses the period parameter for the web page: "week"
// (default, last 7 days) or "month" (last 30 days); anything else falls back
// to week, mirroring the web chart's days fallback (the API twin rejects
// invalid values with 400 instead).
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
func analyticsPeriod(r *http.Request) (string, int) {
	if r.URL.Query().Get("period") == "month" {
		return "month", 30
	}
	return "week", 7
}

// Show renders the global analytics dashboard. The default aggregation scope
// — for every viewer, admins included — is the personal scope: links the
// viewer owns or co-owns plus links shared with them, resolved in the store
// layer. An explicit scope=all query parameter (with a visible toggle shown
// only to admins) switches to instance-wide aggregates; scope=all from a
// non-admin renders the forbidden page.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces", ADR-0021 (f)
func (h *AnalyticsHandler) Show(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/auth/login?return_url="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}

	period, periodDays := analyticsPeriod(r)

	// scope=all is admin-only, per SPEC-0010's explicit admin-override style.
	// Any other scope value falls back to the personal "mine" scope — the
	// fallback can only narrow, never widen — mirroring analyticsPeriod's web
	// fallback pattern (the API twin rejects unrecognized scopes with 400
	// instead).
	// Governing: SPEC-0021 REQ "Global Analytics Dashboard" — scenario "Admin toggle"
	scope := "mine"
	if r.URL.Query().Get("scope") == "all" {
		if !user.IsAdmin() {
			RenderForbidden(w, r)
			return
		}
		scope = "all"
	}

	// The scoped link-ID set is resolved in the store layer from link_owners
	// + link_shares; every panel query is constrained to it, so other users'
	// links — including their public links — contribute nothing.
	// Governing: SPEC-0021 REQ "Global Analytics Dashboard", Security
	// Requirements "Cross-User Aggregation Leakage"
	var scopeIDs []string
	if scope != "all" {
		var err error
		scopeIDs, err = h.clicks.AnalyticsScopeLinkIDs(r.Context(), user.ID)
		if err != nil {
			http.Error(w, "could not load analytics", http.StatusInternalServerError)
			return
		}
	}

	g, err := h.clicks.GetGlobalAnalytics(r.Context(), scopeIDs, scope == "all", periodDays)
	if err != nil {
		http.Error(w, "could not load analytics", http.StatusInternalServerError)
		return
	}

	rows := make([]AnalyticsTopLinkRow, 0, len(g.TopLinks))
	for _, tl := range g.TopLinks {
		row := AnalyticsTopLinkRow{
			LinkID:        tl.LinkID,
			Slug:          tl.Slug,
			Count:         tl.Count,
			PreviousCount: tl.PreviousCount,
		}
		// Trend vs the previous equal-length period: percentage change, or the
		// "new" marker when the previous period count is zero — never a
		// division error.
		// Governing: SPEC-0021 REQ "Global Analytics Dashboard" — scenario
		// "Trend against previous period"
		if tl.PreviousCount == 0 {
			row.IsNew = true
		} else {
			pct := 100 * float64(tl.Count-tl.PreviousCount) / float64(tl.PreviousCount)
			row.TrendUp = pct >= 0
			row.TrendLabel = fmt.Sprintf("%+.1f%%", pct)
		}
		rows = append(rows, row)
	}

	data := AnalyticsPage{
		BasePage:      newBasePage(r, user),
		User:          user,
		Period:        period,
		PeriodDays:    periodDays,
		Scope:         scope,
		IsAdmin:       user.IsAdmin(),
		TopLinks:      rows,
		NeverClicked:  g.NeverClicked,
		TopReferrers:  g.TopReferrers,
		RetentionDays: h.retention,
	}

	if isHTMX(r) {
		renderPageFragment(w, "analytics.html", "content", data)
		return
	}
	render(w, "analytics.html", data)
}
