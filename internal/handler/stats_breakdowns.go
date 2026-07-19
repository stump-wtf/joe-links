// Governing: SPEC-0021 REQ "Click Breakdowns", ADR-0021
package handler

import (
	"fmt"

	"github.com/joestump/joe-links/internal/store"
)

// StatsBreakdownsData is the template payload for the stats_breakdowns
// partial: the three counts-only breakdown tables rendered below the daily
// chart over the same 30/90-day window. It carries no user identities, so the
// same payload serves every CanStats viewer, share recipients included.
// Governing: SPEC-0021 REQ "Click Breakdowns", REQ "Capability Gating of Analytics Surfaces"
type StatsBreakdownsData struct {
	LinkID string
	Days   int
	// OOB marks the render that rides along with the chart window-toggle
	// fragment: the partial's root carries hx-swap-oob so HTMX swaps the
	// breakdown panel to the same window the chart just switched to.
	OOB       bool
	Referrers []store.BreakdownRow
	Browsers  []store.BreakdownRow
	OS        []store.BreakdownRow
	Auth      store.AuthBreakdown
	Total     int64  // Auth.Authenticated + Auth.Anonymous
	AuthPct   string // preformatted, e.g. "41.5%"; empty when Total is 0
	AnonPct   string
}

// newStatsBreakdownsData builds the view model, precomputing the
// authenticated/anonymous percentages (SPEC-0021's breakdown table 3 shows
// both counts and percentages).
// Governing: SPEC-0021 REQ "Click Breakdowns"
func newStatsBreakdownsData(linkID string, days int, oob bool, b store.ClickBreakdowns) StatsBreakdownsData {
	d := StatsBreakdownsData{
		LinkID:    linkID,
		Days:      days,
		OOB:       oob,
		Referrers: b.Referrers,
		Browsers:  b.Browsers,
		OS:        b.OS,
		Auth:      b.Auth,
		Total:     b.Auth.Authenticated + b.Auth.Anonymous,
	}
	if d.Total > 0 {
		d.AuthPct = fmt.Sprintf("%.1f%%", 100*float64(b.Auth.Authenticated)/float64(d.Total))
		d.AnonPct = fmt.Sprintf("%.1f%%", 100*float64(b.Auth.Anonymous)/float64(d.Total))
	}
	return d
}
