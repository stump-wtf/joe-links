// Governing: SPEC-0016 REQ "Click Data Schema", REQ "Click Recording", ADR-0016
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// truncateRunes returns s limited to at most n Unicode code points, never
// splitting a multi-byte character.
func truncateRunes(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ClickEvent represents a single click to be recorded.
type ClickEvent struct {
	LinkID    string
	UserID    string // empty string = anonymous
	IPHash    string // caller computes this
	UserAgent string
	Referrer  string
}

// ClickStats holds aggregate click counts for a link.
type ClickStats struct {
	Total   int64
	Last7d  int64
	Last30d int64
}

// RecentClick represents a single click with optional user info.
type RecentClick struct {
	ID          string    `db:"id"`
	ClickedAt   time.Time `db:"clicked_at"`
	Referrer    string    `db:"referrer"`
	UserID      string    `db:"user_id"`
	DisplayName string    `db:"display_name"`
}

// ClickStore is the sqlx-backed store for click tracking operations.
type ClickStore struct {
	db *sqlx.DB
}

// NewClickStore creates a new ClickStore.
func NewClickStore(db *sqlx.DB) *ClickStore {
	return &ClickStore{db: db}
}

// q rebinds ? placeholders to the driver's native format.
func (s *ClickStore) q(query string) string { return s.db.Rebind(query) }

// RecordClick inserts a click event row.
// Governing: SPEC-0016 REQ "Click Recording", ADR-0016
func (s *ClickStore) RecordClick(ctx context.Context, e ClickEvent) error {
	id := uuid.New().String()
	now := time.Now().UTC()

	// Governing: SPEC-0016 REQ "Click Data Schema" — user_agent/referrer are TEXT
	// columns; length limits (512 / 2048 characters) are enforced here in the
	// application layer, not by a DB constraint. Truncation is rune-aware so a
	// multi-byte character is never split into invalid UTF-8.
	ua := truncateRunes(e.UserAgent, 512)
	ref := truncateRunes(e.Referrer, 2048)

	var userID interface{}
	if e.UserID != "" {
		userID = e.UserID
	}

	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO link_clicks (id, link_id, user_id, ip_hash, user_agent, referrer, clicked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`), id, e.LinkID, userID, e.IPHash, ua, ref, now)
	return err
}

// GetClickStats returns total, 7d, and 30d click counts for a link.
// Governing: SPEC-0016 REQ "Click Data Schema", ADR-0016
func (s *ClickStore) GetClickStats(ctx context.Context, linkID string) (ClickStats, error) {
	var stats ClickStats
	now := time.Now().UTC()
	since7d := now.AddDate(0, 0, -7)
	since30d := now.AddDate(0, 0, -30)

	err := s.db.GetContext(ctx, &stats.Total,
		s.q(`SELECT COUNT(*) FROM link_clicks WHERE link_id = ?`), linkID)
	if err != nil {
		return stats, err
	}

	err = s.db.GetContext(ctx, &stats.Last7d,
		s.q(`SELECT COUNT(*) FROM link_clicks WHERE link_id = ? AND clicked_at >= ?`), linkID, since7d)
	if err != nil {
		return stats, err
	}

	err = s.db.GetContext(ctx, &stats.Last30d,
		s.q(`SELECT COUNT(*) FROM link_clicks WHERE link_id = ? AND clicked_at >= ?`), linkID, since30d)
	if err != nil {
		return stats, err
	}

	return stats, nil
}

// ListRecentClicks returns the most recent N clicks for a link, joining users for display_name.
// Governing: SPEC-0016 REQ "Click Data Schema", ADR-0016
func (s *ClickStore) ListRecentClicks(ctx context.Context, linkID string, limit int) ([]RecentClick, error) {
	var clicks []RecentClick
	err := s.db.SelectContext(ctx, &clicks, s.q(`
		SELECT c.id,
		       c.clicked_at,
		       COALESCE(c.referrer, '') AS referrer,
		       COALESCE(c.user_id, '') AS user_id,
		       COALESCE(u.display_name, '') AS display_name
		FROM link_clicks c
		LEFT JOIN users u ON u.id = c.user_id
		WHERE c.link_id = ?
		ORDER BY c.clicked_at DESC, c.id DESC
		LIMIT ?
	`), linkID, limit)
	if err != nil {
		return nil, err
	}
	return clicks, nil
}

// ListRecentClicksBefore returns clicks for a link strictly before the given
// keyset position, newest first. Rows are ordered by (clicked_at, id)
// descending so the cursor has a deterministic tiebreaker when multiple rows
// share a timestamp (MySQL TIMESTAMP is second-precision, so bursts collide).
// If before is zero, returns from the most recent. If beforeID is empty
// (legacy timestamp-only cursor), falls back to a strict clicked_at
// comparison — rows sharing the boundary timestamp may be skipped, which
// matches the pre-tiebreaker behavior for old cursors.
// Governing: SPEC-0016 REQ "REST API Clicks Endpoint", SPEC-0005 REQ "Pagination", ADR-0016
func (s *ClickStore) ListRecentClicksBefore(ctx context.Context, linkID string, before time.Time, beforeID string, limit int) ([]RecentClick, error) {
	var clicks []RecentClick
	if before.IsZero() {
		err := s.db.SelectContext(ctx, &clicks, s.q(`
			SELECT c.id,
			       c.clicked_at,
			       COALESCE(c.referrer, '') AS referrer,
			       COALESCE(c.user_id, '') AS user_id,
			       COALESCE(u.display_name, '') AS display_name
			FROM link_clicks c
			LEFT JOIN users u ON u.id = c.user_id
			WHERE c.link_id = ?
			ORDER BY c.clicked_at DESC, c.id DESC
			LIMIT ?
		`), linkID, limit)
		if err != nil {
			return nil, err
		}
		return clicks, nil
	}

	if beforeID == "" {
		err := s.db.SelectContext(ctx, &clicks, s.q(`
			SELECT c.id,
			       c.clicked_at,
			       COALESCE(c.referrer, '') AS referrer,
			       COALESCE(c.user_id, '') AS user_id,
			       COALESCE(u.display_name, '') AS display_name
			FROM link_clicks c
			LEFT JOIN users u ON u.id = c.user_id
			WHERE c.link_id = ? AND c.clicked_at < ?
			ORDER BY c.clicked_at DESC, c.id DESC
			LIMIT ?
		`), linkID, before, limit)
		if err != nil {
			return nil, err
		}
		return clicks, nil
	}

	err := s.db.SelectContext(ctx, &clicks, s.q(`
		SELECT c.id,
		       c.clicked_at,
		       COALESCE(c.referrer, '') AS referrer,
		       COALESCE(c.user_id, '') AS user_id,
		       COALESCE(u.display_name, '') AS display_name
		FROM link_clicks c
		LEFT JOIN users u ON u.id = c.user_id
		WHERE c.link_id = ? AND (c.clicked_at < ? OR (c.clicked_at = ? AND c.id < ?))
		ORDER BY c.clicked_at DESC, c.id DESC
		LIMIT ?
	`), linkID, before, before, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return clicks, nil
}

// DailyClickCount is one UTC-calendar-day bucket in a link's click time
// series. Pruned distinguishes a day older than the retention horizon
// (no-data — the rows were deleted) from a genuinely unclicked day (zero):
// a pruned day is not an unclicked day.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
type DailyClickCount struct {
	Date   string // UTC calendar day, "2006-01-02"
	Count  int64
	Pruned bool // day opens before the retention horizon: render as no-data, not zero
}

// GetDailyClickSeries returns exactly days (30 or 90) UTC-day buckets for a
// link, ascending by date, ending on today's (partial) UTC day. Gap days are
// present with zero counts — consumers never interpolate. retentionDays > 0
// marks buckets opening before the retention horizon (now − retentionDays)
// as Pruned; 0 disables the marker (retention off, the default).
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
func (s *ClickStore) GetDailyClickSeries(ctx context.Context, linkID string, days, retentionDays int) ([]DailyClickCount, error) {
	return s.dailyClickSeries(ctx, linkID, days, retentionDays, time.Now().UTC())
}

// dailyClickSeries is GetDailyClickSeries with an injectable clock for tests.
//
// Index strategy: one indexed range scan on idx_link_clicks_link_id_clicked_at
// (link_id, clicked_at DESC — migration 00012); no full-table scan regardless
// of how hot the link is, and no new index or migration is needed. Bucketing
// happens in Go — no SQL date functions (strftime/DATE_FORMAT/to_char), the
// most dialect-divergent SQL there is (ADR-0002, ADR-0021). Rows stream
// through a bounded counts map; the fetched timestamps are never materialized
// as a full in-memory slice.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021, ADR-0002
func (s *ClickStore) dailyClickSeries(ctx context.Context, linkID string, days, retentionDays int, now time.Time) ([]DailyClickCount, error) {
	// The only valid windows are 30 and 90; callers own their own policy
	// (API: 400, web UI: fall back to 30) before reaching the store.
	// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
	if days != 30 && days != 90 {
		return nil, fmt.Errorf("invalid time series window %d days: must be 30 or 90", days)
	}

	// Window boundaries are pinned: buckets are whole UTC calendar days; the
	// window is today's UTC day (partial until the day ends) plus the
	// preceding days−1 whole days; the SQL range predicate is aligned to the
	// UTC midnight opening the oldest bucket — never a rolling now−Nd bound.
	// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	windowStart := today.AddDate(0, 0, -(days - 1))

	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT clicked_at FROM link_clicks
		WHERE link_id = ? AND clicked_at >= ?
	`), linkID, windowStart)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// Bucket by UTC calendar day in Go, streaming over the result rows with
	// counts accumulated in bounded memory (one entry per distinct day).
	counts := make(map[string]int64, days)
	for rows.Next() {
		var clickedAt time.Time
		if err := rows.Scan(&clickedAt); err != nil {
			return nil, err
		}
		counts[clickedAt.UTC().Format("2006-01-02")]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Retention horizon: a bucket whose UTC day opens before now − retention
	// is (at least partially) pruned territory — no-data, not zero. With a
	// 60-day horizon and a 90-day window this marks exactly the oldest 30
	// day positions.
	// Governing: SPEC-0021 REQ "Per-Link Daily Time Series" — pruned days
	// distinguished from zero days; REQ "Click Retention"
	var horizon time.Time
	if retentionDays > 0 {
		horizon = now.AddDate(0, 0, -retentionDays)
	}

	series := make([]DailyClickCount, 0, days)
	for d := 0; d < days; d++ {
		day := windowStart.AddDate(0, 0, d)
		date := day.Format("2006-01-02")
		series = append(series, DailyClickCount{
			Date:   date,
			Count:  counts[date],
			Pruned: retentionDays > 0 && day.Before(horizon),
		})
	}
	return series, nil
}

// HashIP computes SHA-256(ip + ":" + YYYYMMDD_UTC) for the current day.
// Governing: SPEC-0016 REQ "Click Recording", ADR-0016
func HashIP(ip string) string {
	salt := time.Now().UTC().Format("20060102")
	h := sha256.Sum256([]byte(ip + ":" + salt))
	return fmt.Sprintf("%x", h)
}
