// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces", ADR-0021
package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"
)

// dashboardTopN caps the top-links and busiest-referrers dashboard panels.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
const dashboardTopN = 10

// neverClickedCap bounds the never-clicked dashboard panel.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
const neverClickedCap = 10

// neverClickedMinAge is the creation grace period before a link may appear in
// the never-clicked panel: links created at least this long ago qualify.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
const neverClickedMinAge = 7 * 24 * time.Hour

// TopLinkStat is one row in the dashboard's top-links panel: a scope link's
// click count for the current period plus the previous equal-length period's
// count, from which callers derive the trend. Counts only — no clicker
// identities anywhere.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
type TopLinkStat struct {
	LinkID        string
	Slug          string
	Count         int64
	PreviousCount int64
}

// NeverClickedLink is one row in the dashboard's never-clicked panel.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
type NeverClickedLink struct {
	LinkID    string    `db:"id"`
	Slug      string    `db:"slug"`
	CreatedAt time.Time `db:"created_at"`
}

// GlobalAnalytics holds the three dashboard panels. Aggregate counts only —
// no field anywhere in it names or identifies any user.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces"
type GlobalAnalytics struct {
	TopLinks     []TopLinkStat
	NeverClicked []NeverClickedLink
	TopReferrers []BreakdownRow // by host, top-10 + "Other", per REQ "Click Breakdowns"
}

// AnalyticsScopeLinkIDs resolves the viewer's personal dashboard scope in the
// store layer: links the viewer owns or co-owns (link_owners) plus links
// shared with the viewer (link_shares). This enumeration — not the CanStats
// predicate — defines the personal scope: for non-admins the two coincide,
// and for admins (whose CanStats is universal) the personal scope is
// deliberately narrower; instance-wide aggregates exist only behind the
// explicit scope=all toggle. Other users' links, including their public
// links, never appear here: public grants resolvability and browsability,
// not stats access.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability
// Gating of Analytics Surfaces", Security Requirements "Cross-User
// Aggregation Leakage", ADR-0021 (f)
func (s *ClickStore) AnalyticsScopeLinkIDs(ctx context.Context, userID string) ([]string, error) {
	var ids []string
	err := s.db.SelectContext(ctx, &ids, s.q(`
		SELECT link_id FROM link_owners WHERE user_id = ?
		UNION
		SELECT link_id FROM link_shares WHERE user_id = ?
	`), userID, userID)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// GetGlobalAnalytics computes the dashboard panels for the given link scope
// over the last periodDays (7 for week, 30 for month) plus the previous
// equal-length period for trends. When all is true the aggregates are
// instance-wide (the admin-only scope=all view) and scope is ignored;
// otherwise every panel query is constrained to the scope ID set in SQL —
// there is no code path where an unconstrained aggregate is filtered
// afterward in a handler.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", Security
// Requirements "Cross-User Aggregation Leakage", ADR-0021 (a)/(f)
func (s *ClickStore) GetGlobalAnalytics(ctx context.Context, scope []string, all bool, periodDays int) (GlobalAnalytics, error) {
	return s.globalAnalytics(ctx, scope, all, periodDays, time.Now().UTC())
}

// globalAnalytics is GetGlobalAnalytics with an injectable clock for tests.
//
// Window boundaries follow the pinned time-series style: whole UTC calendar
// days, with today's partial UTC day as the newest bucket. The current period
// is today plus the preceding periodDays−1 whole days; the previous period is
// the periodDays whole days before that. Day bucketing, host grouping, and
// ranking happen in Go over portable SQL — no SQL date or string functions
// (ADR-0002); per-row streams accumulate into bounded maps.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", ADR-0021 (a), ADR-0002
func (s *ClickStore) globalAnalytics(ctx context.Context, scope []string, all bool, periodDays int, now time.Time) (GlobalAnalytics, error) {
	g := GlobalAnalytics{
		TopLinks:     []TopLinkStat{},
		NeverClicked: []NeverClickedLink{},
		TopReferrers: []BreakdownRow{},
	}

	if periodDays != 7 && periodDays != 30 {
		return g, fmt.Errorf("invalid analytics period %d days: must be 7 (week) or 30 (month)", periodDays)
	}

	// An empty personal scope means every panel is empty by construction — no
	// query to run, and nothing another user owns can leak in.
	if !all && len(scope) == 0 {
		return g, nil
	}

	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	periodStart := today.AddDate(0, 0, -(periodDays - 1))
	prevStart := periodStart.AddDate(0, 0, -periodDays)

	// Panel 1: top links — current-period counts per link (portable COUNT +
	// GROUP BY over the scoped ID set), previous-period counts for the trend.
	type linkCount struct {
		slug  string
		count int64
	}
	current := make(map[string]linkCount)
	{
		query := `
			SELECT c.link_id, l.slug, COUNT(*)
			FROM link_clicks c
			INNER JOIN links l ON l.id = c.link_id
			WHERE c.clicked_at >= ?`
		args := []interface{}{periodStart}
		if !all {
			var err error
			query, args, err = sqlx.In(query+` AND c.link_id IN (?)`, periodStart, scope)
			if err != nil {
				return g, err
			}
		}
		rows, err := s.db.QueryContext(ctx, s.q(query+` GROUP BY c.link_id, l.slug`), args...)
		if err != nil {
			return g, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, slug string
			var n int64
			if err := rows.Scan(&id, &slug, &n); err != nil {
				return g, err
			}
			current[id] = linkCount{slug: slug, count: n}
		}
		if err := rows.Err(); err != nil {
			return g, err
		}
	}

	previous := make(map[string]int64)
	{
		query := `
			SELECT link_id, COUNT(*)
			FROM link_clicks
			WHERE clicked_at >= ? AND clicked_at < ?`
		args := []interface{}{prevStart, periodStart}
		if !all {
			var err error
			query, args, err = sqlx.In(query+` AND link_id IN (?)`, prevStart, periodStart, scope)
			if err != nil {
				return g, err
			}
		}
		rows, err := s.db.QueryContext(ctx, s.q(query+` GROUP BY link_id`), args...)
		if err != nil {
			return g, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				return g, err
			}
			previous[id] = n
		}
		if err := rows.Err(); err != nil {
			return g, err
		}
	}

	for id, lc := range current {
		g.TopLinks = append(g.TopLinks, TopLinkStat{
			LinkID:        id,
			Slug:          lc.slug,
			Count:         lc.count,
			PreviousCount: previous[id],
		})
	}
	// Most-clicked first; slug ascending on ties for deterministic output.
	sort.Slice(g.TopLinks, func(i, j int) bool {
		if g.TopLinks[i].Count != g.TopLinks[j].Count {
			return g.TopLinks[i].Count > g.TopLinks[j].Count
		}
		return g.TopLinks[i].Slug < g.TopLinks[j].Slug
	})
	if len(g.TopLinks) > dashboardTopN {
		g.TopLinks = g.TopLinks[:dashboardTopN]
	}

	// Panel 2: never-clicked — scope links past the 7-day creation grace with
	// zero recorded clicks, newest first, capped. Lifecycle state is
	// deliberately not consulted (SPEC-0021 owns no archived-link special
	// case). With retention enabled "never clicked" means "no clicks within
	// the retention horizon"; the UI labels it so.
	{
		cutoff := now.Add(-neverClickedMinAge)
		query := `
			SELECT l.id, l.slug, l.created_at
			FROM links l
			WHERE l.created_at <= ?
			  AND NOT EXISTS (SELECT 1 FROM link_clicks c WHERE c.link_id = l.id)`
		args := []interface{}{cutoff}
		if !all {
			var err error
			query, args, err = sqlx.In(query+` AND l.id IN (?)`, cutoff, scope)
			if err != nil {
				return g, err
			}
		}
		err := s.db.SelectContext(ctx, &g.NeverClicked, s.q(query+`
			ORDER BY l.created_at DESC, l.slug ASC
			LIMIT `+fmt.Sprintf("%d", neverClickedCap)), args...)
		if err != nil {
			return g, err
		}
	}

	// Panel 3: busiest referrers — hosts grouped in Go per REQ "Click
	// Breakdowns" (url.Parse host extraction is not expressible in portable
	// SQL), streaming over the rows with counts in a bounded map.
	{
		query := `
			SELECT COALESCE(referrer, '')
			FROM link_clicks
			WHERE clicked_at >= ?`
		args := []interface{}{periodStart}
		if !all {
			var err error
			query, args, err = sqlx.In(query+` AND link_id IN (?)`, periodStart, scope)
			if err != nil {
				return g, err
			}
		}
		rows, err := s.db.QueryContext(ctx, s.q(query), args...)
		if err != nil {
			return g, err
		}
		defer func() { _ = rows.Close() }()
		refCounts := make(map[string]int64)
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				return g, err
			}
			refCounts[referrerHost(ref)]++
		}
		if err := rows.Err(); err != nil {
			return g, err
		}
		g.TopReferrers = topNPlusOther(refCounts, dashboardTopN)
	}

	return g, nil
}
