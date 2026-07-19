// Destination-health state for links: the link_health table written by the
// background checker, the derived health states surfaced on badges/REST/MCP,
// and the per-link opt-out flag. All derivation lives here in the shared
// store layer so web, REST, and MCP can never disagree on what "broken"
// means (ADR-0020).
//
// Governing: SPEC-0020 REQ "Destination Health Checking", REQ "Health Badges
// and Admin Report", REQ "Lifecycle State in API and MCP", ADR-0020
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// Derived destination-health states. HealthUnchecked is declared next to the
// lifecycle constants in link_store.go.
// Governing: SPEC-0020 REQ "Destination Health Checking" — State
const (
	HealthOK      = "ok"
	HealthBroken  = "broken"
	HealthSkipped = "skipped"
)

// HealthBrokenThreshold is the consecutive-failure count at which a link is
// reported broken. Broken is derived from the counter, never stored — the
// same no-drift principle as the lifecycle timestamps (ADR-0020).
// Governing: SPEC-0020 REQ "Destination Health Checking" — Politeness
const HealthBrokenThreshold = 3

// HealthBackoffCap caps the failure backoff at this many intervals.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Politeness
const HealthBackoffCap = 7

// Staleness windows for the dashboard staleness views. Both are computed from
// link_clicks at query time — no denormalized counters in v1. Epic #216's
// click-retention pruning must keep at least StaleWindow of history or these
// views go silently wrong (coupling recorded in ADR-0020).
// Governing: SPEC-0020 REQ "Staleness Views"
const (
	StaleWindow       = 90 * 24 * time.Hour
	NeverClickedGrace = 7 * 24 * time.Hour
)

// LinkHealth is one row of the link_health table: the persisted outcome of
// the most recent destination probe for a link. Absence of a row means
// "never checked".
// Governing: SPEC-0020 REQ "Destination Health Checking" — State
type LinkHealth struct {
	LinkID              string     `db:"link_id"`
	LastCheckedAt       *time.Time `db:"last_checked_at"`
	LastStatus          *int       `db:"last_status"`
	LastError           *string    `db:"last_error"`
	ConsecutiveFailures int        `db:"consecutive_failures"`
	NextCheckAt         *time.Time `db:"next_check_at"`
	Skipped             bool       `db:"skipped"`
}

// HealthView is the surfaced health state for a link, after the SPEC-0020
// surfacing rule has been applied. This — not the raw LinkHealth row — is
// what badges, REST, and MCP render.
type HealthView struct {
	Status        string
	LastCheckedAt *time.Time
	LastStatus    *int
}

// DeriveHealth applies the SPEC-0020 derivation and surfacing rule: health is
// surfaced only while the link is eligible for checking. For an opted-out,
// archived, or expired link the frozen link_health row is NOT surfaced — the
// status reports "unchecked" with null details, and no badge renders.
// Otherwise: skipped when the skipped flag is set; else broken when
// consecutive_failures >= HealthBrokenThreshold; else ok when a row exists;
// unchecked when none does.
// Governing: SPEC-0020 REQ "Destination Health Checking" — State, surfacing rule
func DeriveHealth(link *Link, h *LinkHealth, now time.Time) HealthView {
	if link.HealthChecksDisabled || link.IsArchived() || link.IsExpired(now) || h == nil {
		return HealthView{Status: HealthUnchecked}
	}
	switch {
	case h.Skipped:
		return HealthView{Status: HealthSkipped, LastCheckedAt: h.LastCheckedAt, LastStatus: h.LastStatus}
	case h.ConsecutiveFailures >= HealthBrokenThreshold:
		return HealthView{Status: HealthBroken, LastCheckedAt: h.LastCheckedAt, LastStatus: h.LastStatus}
	default:
		return HealthView{Status: HealthOK, LastCheckedAt: h.LastCheckedAt, LastStatus: h.LastStatus}
	}
}

// HealthBackoff returns how far out the next check must be scheduled after
// the given consecutive-failure count: interval × 2^(n−1), capped at
// HealthBackoffCap × interval. Healthy links are checked once per interval.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Politeness
func HealthBackoff(interval time.Duration, failures int) time.Duration {
	if failures < 1 {
		return interval
	}
	backoff := interval
	for i := 1; i < failures; i++ {
		backoff *= 2
		if backoff >= HealthBackoffCap*interval {
			return HealthBackoffCap * interval
		}
	}
	if backoff > HealthBackoffCap*interval {
		backoff = HealthBackoffCap * interval
	}
	return backoff
}

// SetHealthChecksDisabled toggles the per-link health-check opt-out. The flag
// is owner intent (an edit — callers gate with LinkCaps.CanEdit), so it lives
// on links and bumps updated_at like any other edit. The frozen link_health
// row is left in place; DeriveHealth stops surfacing it.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Eligibility,
// scenario "Opt-Out Honored"
func (s *LinkStore) SetHealthChecksDisabled(ctx context.Context, id string, disabled bool) (*Link, error) {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE links SET health_checks_disabled = ?, updated_at = ? WHERE id = ?`),
		disabled, now, id)
	if err != nil {
		return nil, err
	}
	return s.GetByID(ctx, id)
}

// GetHealth returns the link_health row for a link, or (nil, nil) when the
// link has never been checked.
func (s *LinkStore) GetHealth(ctx context.Context, linkID string) (*LinkHealth, error) {
	var h LinkHealth
	err := s.db.GetContext(ctx, &h, s.q(`SELECT * FROM link_health WHERE link_id = ?`), linkID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// HealthForLinks returns link_health rows for the given link IDs in one
// batched query (list renders re-run on every HTMX keystroke, so per-row
// queries compound fast). IDs with no row are absent from the map.
func (s *LinkStore) HealthForLinks(ctx context.Context, linkIDs []string) (map[string]*LinkHealth, error) {
	out := make(map[string]*LinkHealth, len(linkIDs))
	if len(linkIDs) == 0 {
		return out, nil
	}
	query, args, err := sqlx.In(`SELECT * FROM link_health WHERE link_id IN (?)`, linkIDs)
	if err != nil {
		return nil, err
	}
	var rows []*LinkHealth
	if err := s.db.SelectContext(ctx, &rows, s.db.Rebind(query), args...); err != nil {
		return nil, err
	}
	for _, h := range rows {
		out[h.LinkID] = h
	}
	return out, nil
}

// upsertHealth inserts or updates the link_health row for linkID. The checker
// is single-instance (ADR-0020), so a portable read-then-write is used
// instead of dialect-divergent ON CONFLICT clauses (ADR-0002).
func (s *LinkStore) upsertHealth(ctx context.Context, linkID string, apply func(h *LinkHealth)) (*LinkHealth, error) {
	h, err := s.GetHealth(ctx, linkID)
	if err != nil {
		return nil, err
	}
	exists := h != nil
	if h == nil {
		h = &LinkHealth{LinkID: linkID}
	}
	apply(h)
	if exists {
		_, err = s.db.ExecContext(ctx, s.q(`
			UPDATE link_health
			SET last_checked_at = ?, last_status = ?, last_error = ?, consecutive_failures = ?, next_check_at = ?, skipped = ?
			WHERE link_id = ?
		`), h.LastCheckedAt, h.LastStatus, h.LastError, h.ConsecutiveFailures, h.NextCheckAt, h.Skipped, linkID)
	} else {
		_, err = s.db.ExecContext(ctx, s.q(`
			INSERT INTO link_health (link_id, last_checked_at, last_status, last_error, consecutive_failures, next_check_at, skipped)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`), linkID, h.LastCheckedAt, h.LastStatus, h.LastError, h.ConsecutiveFailures, h.NextCheckAt, h.Skipped)
	}
	if err != nil {
		return nil, err
	}
	return h, nil
}

// RecordHealthSuccess records a successful probe: status and time stored,
// consecutive_failures reset to zero, skipped cleared (a completed probe
// clears the flag), next check one interval out. The checker never writes to
// the links table (no updated_at churn — ADR-0020).
// Governing: SPEC-0020 REQ "Destination Health Checking" scenario "Healthy Destination"
func (s *LinkStore) RecordHealthSuccess(ctx context.Context, linkID string, status int, checkedAt time.Time, interval time.Duration) (*LinkHealth, error) {
	next := checkedAt.Add(interval)
	return s.upsertHealth(ctx, linkID, func(h *LinkHealth) {
		h.LastCheckedAt = &checkedAt
		h.LastStatus = &status
		h.LastError = nil
		h.ConsecutiveFailures = 0
		h.NextCheckAt = &next
		h.Skipped = false
	})
}

// RecordHealthFailure records a failed probe (4xx/5xx, network error,
// timeout, or redirect-limit exhaustion): the counter increments and the next
// check backs off exponentially per HealthBackoff. Returns the updated row so
// the caller can observe the new failure count.
// Governing: SPEC-0020 REQ "Destination Health Checking" scenario "Failures
// Back Off and Eventually Mark Broken"
func (s *LinkStore) RecordHealthFailure(ctx context.Context, linkID string, status *int, errMsg string, checkedAt time.Time, interval time.Duration) (*LinkHealth, error) {
	return s.upsertHealth(ctx, linkID, func(h *LinkHealth) {
		h.LastCheckedAt = &checkedAt
		h.LastStatus = status
		if errMsg != "" {
			h.LastError = &errMsg
		} else {
			h.LastError = nil
		}
		h.ConsecutiveFailures++
		next := checkedAt.Add(HealthBackoff(interval, h.ConsecutiveFailures))
		h.NextCheckAt = &next
		h.Skipped = false
	})
}

// DeferHealthCheck records a 429 Too Many Requests response: NOT a failure —
// consecutive_failures is neither incremented nor reset — but the next check
// is pushed out by delay (at least one full interval, honoring a larger
// Retry-After; the caller computes the max). The 429 is still a completed
// probe, so last_checked_at/last_status update and skipped clears.
// Governing: SPEC-0020 REQ "Destination Health Checking" scenario "429 Is Not a Failure"
func (s *LinkStore) DeferHealthCheck(ctx context.Context, linkID string, status int, checkedAt time.Time, delay time.Duration) (*LinkHealth, error) {
	next := checkedAt.Add(delay)
	return s.upsertHealth(ctx, linkID, func(h *LinkHealth) {
		h.LastCheckedAt = &checkedAt
		h.LastStatus = &status
		h.LastError = nil
		h.NextCheckAt = &next
		h.Skipped = false
	})
}

// RecordHealthSkipped records a probe attempt that was skipped because the
// destination's scheme or resolved address is not checkable under current
// policy (SSRF guard, non-http(s) scheme). Skipped is never broken — the
// counter is untouched — and the link is re-considered one interval out in
// case policy changes.
// Governing: SPEC-0020 "Security Requirements" — SSRF Resistance; REQ
// "Destination Health Checking" — State (skipped flag)
func (s *LinkStore) RecordHealthSkipped(ctx context.Context, linkID, reason string, checkedAt time.Time, interval time.Duration) (*LinkHealth, error) {
	next := checkedAt.Add(interval)
	return s.upsertHealth(ctx, linkID, func(h *LinkHealth) {
		h.LastCheckedAt = &checkedAt
		h.LastStatus = nil
		if reason != "" {
			h.LastError = &reason
		} else {
			h.LastError = nil
		}
		h.NextCheckAt = &next
		h.Skipped = true
	})
}

// ListDueForHealthCheck returns links eligible for a health probe this cycle:
// due (no link_health row, or next_check_at absent or <= now) and not
// archived, not expired, not opted out. Variable links (SPEC-0009 — a URL
// template is not a fetchable destination) are filtered here too, so no
// caller can fetch one. No fetch may be issued for any link this method does
// not return, and skipped links are never marked broken.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Eligibility
func (s *LinkStore) ListDueForHealthCheck(ctx context.Context, now time.Time) ([]*Link, error) {
	var rows []*Link
	err := s.db.SelectContext(ctx, &rows, s.q(`
		SELECT l.* FROM links l
		LEFT JOIN link_health lh ON lh.link_id = l.id
		WHERE l.health_checks_disabled = ?
		  AND `+linkActiveWhere+`
		  AND (lh.link_id IS NULL OR lh.next_check_at IS NULL OR lh.next_check_at <= ?)
		ORDER BY l.slug ASC
	`), false, now, now)
	if err != nil {
		return nil, err
	}
	out := make([]*Link, 0, len(rows))
	for _, l := range rows {
		// Governing: SPEC-0009 — variable links are skipped by the checker.
		if VarPlaceholderRe.MatchString(l.URL) {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// BrokenLink is one row of the admin health report: a currently broken link
// with its owner display names and latest check details.
// Governing: SPEC-0020 REQ "Health Badges and Admin Report"
type BrokenLink struct {
	Link
	Owners              string     `db:"owners"`
	LastCheckedAt       *time.Time `db:"last_checked_at"`
	LastStatus          *int       `db:"last_status"`
	LastError           *string    `db:"last_error"`
	ConsecutiveFailures int        `db:"consecutive_failures"`
}

// ListBrokenLinks returns all currently broken links for the admin report at
// /admin/link-health, ordered by most consecutive failures first. Skipped,
// opted-out, archived, and expired links are excluded — their health rows are
// frozen and not surfaced (the surfacing rule), so only genuinely broken,
// still-checked links appear.
// Governing: SPEC-0020 REQ "Health Badges and Admin Report" scenario "Admin
// Report Lists Failing Links"
func (s *LinkStore) ListBrokenLinks(ctx context.Context, now time.Time) ([]*BrokenLink, error) {
	var rows []*BrokenLink
	// GROUP BY the two primary keys: every other selected column is
	// functionally dependent on them, which sqlite, mysql (ONLY_FULL_GROUP_BY),
	// and postgres all accept.
	err := s.db.SelectContext(ctx, &rows, s.q(fmt.Sprintf(`
		SELECT l.*,
		       %s AS owners,
		       lh.last_checked_at, lh.last_status, lh.last_error, lh.consecutive_failures
		FROM links l
		INNER JOIN link_health lh ON lh.link_id = l.id
		LEFT JOIN link_owners lo ON lo.link_id = l.id
		LEFT JOIN users u ON u.id = lo.user_id
		WHERE lh.consecutive_failures >= ?
		  AND lh.skipped = ?
		  AND l.health_checks_disabled = ?
		  AND `+linkActiveWhere+`
		GROUP BY l.id, lh.link_id
		ORDER BY lh.consecutive_failures DESC, l.slug ASC
	`, s.aggDistinct("u.display_name"))), HealthBrokenThreshold, false, false, now)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// ListStaleByOwner returns the viewer's stale links: created more than 90
// days ago with no recorded click in the last 90 days. Archived links are out
// of scope — archiving is a deliberate retirement, not staleness. Computed
// from link_clicks at query time (no denormalized counters in v1).
// Governing: SPEC-0020 REQ "Staleness Views" scenario "Stale Filter"
func (s *LinkStore) ListStaleByOwner(ctx context.Context, ownerID string, now time.Time) ([]*Link, error) {
	cutoff := now.Add(-StaleWindow)
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		INNER JOIN link_owners lo ON lo.link_id = l.id
		WHERE lo.user_id = ?
		  AND l.archived_at IS NULL
		  AND l.created_at <= ?
		  AND NOT EXISTS (
		      SELECT 1 FROM link_clicks c WHERE c.link_id = l.id AND c.clicked_at > ?
		  )
		ORDER BY l.slug ASC
	`), ownerID, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListStaleAll is ListStaleByOwner across all links, for the admin scope.
// Governing: SPEC-0020 REQ "Staleness Views"
func (s *LinkStore) ListStaleAll(ctx context.Context, now time.Time) ([]*Link, error) {
	cutoff := now.Add(-StaleWindow)
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		WHERE l.archived_at IS NULL
		  AND l.created_at <= ?
		  AND NOT EXISTS (
		      SELECT 1 FROM link_clicks c WHERE c.link_id = l.id AND c.clicked_at > ?
		  )
		ORDER BY l.slug ASC
	`), cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListNeverClickedByOwner returns the viewer's never-clicked links: created
// more than 7 days ago (the creation grace period) with no recorded click at
// all. Archived links are out of scope.
// Governing: SPEC-0020 REQ "Staleness Views" scenario "Never-Clicked Filter"
func (s *LinkStore) ListNeverClickedByOwner(ctx context.Context, ownerID string, now time.Time) ([]*Link, error) {
	cutoff := now.Add(-NeverClickedGrace)
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		INNER JOIN link_owners lo ON lo.link_id = l.id
		WHERE lo.user_id = ?
		  AND l.archived_at IS NULL
		  AND l.created_at <= ?
		  AND NOT EXISTS (SELECT 1 FROM link_clicks c WHERE c.link_id = l.id)
		ORDER BY l.slug ASC
	`), ownerID, cutoff)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListNeverClickedAll is ListNeverClickedByOwner across all links (admin scope).
// Governing: SPEC-0020 REQ "Staleness Views"
func (s *LinkStore) ListNeverClickedAll(ctx context.Context, now time.Time) ([]*Link, error) {
	cutoff := now.Add(-NeverClickedGrace)
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		WHERE l.archived_at IS NULL
		  AND l.created_at <= ?
		  AND NOT EXISTS (SELECT 1 FROM link_clicks c WHERE c.link_id = l.id)
		ORDER BY l.slug ASC
	`), cutoff)
	if err != nil {
		return nil, err
	}
	return links, nil
}
