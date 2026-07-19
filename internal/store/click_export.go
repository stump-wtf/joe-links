// Click-history CSV export: one store-level streaming iterator and one CSV
// encoder shared by the paired session (/dashboard) and PAT (/api/v1) routes,
// so both emit byte-identical output for identical inputs (ADR-0021). Rows
// stream oldest-first in bounded keyset batches on (clicked_at, id) — the
// PR #242 pattern — and the full result set is never buffered in memory.
//
// Governing: SPEC-0021 REQ "CSV Export", ADR-0021
package store

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/joestump/joe-links/internal/analytics"
)

// ExportRowCap is the hard per-response row cap. When it truncates an export,
// the response carries an opaque (clicked_at, id) continuation cursor in the
// X-Next-Cursor header — never a bare timestamp, because clicked_at has
// second precision on at least the mysql driver and timestamp ties at the cap
// boundary are routine.
// Governing: SPEC-0021 REQ "CSV Export", Security Requirements "Export Volume Caps"
const ExportRowCap = 100000

// exportBatchSize bounds each keyset page the iterator fetches.
const exportBatchSize = 1000

// clickCSVHeader is the exact export header row and column order.
// Governing: SPEC-0021 REQ "CSV Export"
var clickCSVHeader = []string{"clicked_at", "referrer", "user_agent", "browser", "os", "authenticated", "user"}

// ClickExportRow is one click row as walked by the export iterator.
type ClickExportRow struct {
	ID          string
	ClickedAt   time.Time
	Referrer    string
	UserAgent   string
	UserID      string // empty = anonymous click
	DisplayName string
}

// ClickExportQuery bounds one export request: the link, an optional
// from/to window, and an optional exclusive keyset resume position decoded
// from a continuation cursor.
// Governing: SPEC-0021 REQ "CSV Export"
type ClickExportQuery struct {
	LinkID  string
	From    time.Time // inclusive lower bound; zero = unbounded
	To      time.Time // inclusive upper bound; zero = unbounded
	AfterTS time.Time // exclusive (clicked_at, id) resume position; zero = start
	AfterID string
}

// where builds the portable SQL predicate and args for q. The resume
// predicate is strictly-after in (clicked_at, id) keyset order: no row
// skipped, none duplicated, even across timestamp ties.
// Governing: SPEC-0021 REQ "CSV Export", ADR-0002
func (q ClickExportQuery) where() (string, []interface{}) {
	pred := "c.link_id = ?"
	args := []interface{}{q.LinkID}
	if !q.From.IsZero() {
		pred += " AND c.clicked_at >= ?"
		args = append(args, q.From)
	}
	if !q.To.IsZero() {
		pred += " AND c.clicked_at <= ?"
		args = append(args, q.To)
	}
	if !q.AfterTS.IsZero() {
		pred += " AND (c.clicked_at > ? OR (c.clicked_at = ? AND c.id > ?))"
		args = append(args, q.AfterTS, q.AfterTS, q.AfterID)
	}
	return pred, args
}

// ClickExportNextCursor is the single keyset probe run before streaming
// begins: response headers precede a streamed body, so truncation must be
// determined up front. It returns the keyset position of the cap-th row when
// a row beyond the cap exists (truncated=true); resumption from that position
// is exclusive.
//
// Note: this probe and the subsequent StreamClickExportCSV run as separate,
// non-transactional queries. A click inserted between them whose clicked_at
// ties the cap-boundary row and whose UUID sorts before the boundary id would
// shift the cap-th row, making the already-issued cursor skip exactly one row
// on resume. That requires the 100,000th-oldest row's timestamp to equal the
// current second during the export request — practically unreachable, and the
// probe-before-stream shape is spec-mandated. If export is ever refactored,
// wrap both queries in one transaction/snapshot to close the window.
// Governing: SPEC-0021 REQ "CSV Export"
func (s *ClickStore) ClickExportNextCursor(ctx context.Context, q ClickExportQuery, cap int) (nextTS time.Time, nextID string, truncated bool, err error) {
	pred, args := q.where()
	args = append(args, 2, cap-1)
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT c.clicked_at, c.id
		FROM link_clicks c
		WHERE `+pred+`
		ORDER BY c.clicked_at, c.id
		LIMIT ? OFFSET ?
	`), args...)
	if err != nil {
		return time.Time{}, "", false, err
	}
	defer func() { _ = rows.Close() }()

	var (
		capTS   time.Time
		capID   string
		scanned int
	)
	for rows.Next() {
		if scanned == 0 {
			if err := rows.Scan(&capTS, &capID); err != nil {
				return time.Time{}, "", false, err
			}
		} else {
			// The row beyond the cap exists — its values are irrelevant.
			var ts time.Time
			var id string
			if err := rows.Scan(&ts, &id); err != nil {
				return time.Time{}, "", false, err
			}
		}
		scanned++
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, "", false, err
	}
	if scanned < 2 {
		return time.Time{}, "", false, nil
	}
	return capTS, capID, true, nil
}

// clickExportPage fetches one bounded keyset page, oldest-first, joining
// users for the display name (emitted only for CanManageShares callers).
func (s *ClickStore) clickExportPage(ctx context.Context, q ClickExportQuery, limit int) ([]ClickExportRow, error) {
	pred, args := q.where()
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT c.id,
		       c.clicked_at,
		       COALESCE(c.referrer, '') AS referrer,
		       COALESCE(c.user_agent, '') AS user_agent,
		       COALESCE(c.user_id, '') AS user_id,
		       COALESCE(u.display_name, '') AS display_name
		FROM link_clicks c
		LEFT JOIN users u ON u.id = c.user_id
		WHERE `+pred+`
		ORDER BY c.clicked_at, c.id
		LIMIT ?
	`), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	page := make([]ClickExportRow, 0, limit)
	for rows.Next() {
		var r ClickExportRow
		if err := rows.Scan(&r.ID, &r.ClickedAt, &r.Referrer, &r.UserAgent, &r.UserID, &r.DisplayName); err != nil {
			return nil, err
		}
		page = append(page, r)
	}
	return page, rows.Err()
}

// StreamClickExportCSV writes the header row and up to cap click rows to w as
// CSV, oldest-first in (clicked_at, id) keyset order, reading the table in
// bounded keyset batches — the full result set is never buffered.
//
// includeAttribution gates the raw user_agent and user columns: they are
// populated only for CanManageShares callers, and present-but-empty for
// everyone else (share recipients), because per-row device fingerprints
// correlated with exact timestamps are adjacent to the roster attribution
// PR #255 sealed. The parsed browser/os family columns stay populated for
// every CanStats caller. Every attacker-influenceable cell is CSV-injection
// escaped via csvGuardCell in addition to RFC 4180 quoting.
// Governing: SPEC-0021 REQ "CSV Export", Security Requirements "CSV Injection",
// "Clicker Attribution and the Share Roster"
func (s *ClickStore) StreamClickExportCSV(ctx context.Context, w io.Writer, q ClickExportQuery, cap int, includeAttribution bool) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(clickCSVHeader); err != nil {
		return err
	}

	remaining := cap
	for remaining > 0 {
		batch := exportBatchSize
		if batch > remaining {
			batch = remaining
		}
		page, err := s.clickExportPage(ctx, q, batch)
		if err != nil {
			return err
		}
		for _, row := range page {
			// Read-time UA classification — populated for all callers.
			// Governing: SPEC-0021 REQ "User-Agent Parsing"
			browser, osFamily := analytics.ClassifyUA(row.UserAgent)
			uaCell, userCell := "", ""
			if includeAttribution {
				uaCell = row.UserAgent
				if row.UserID != "" {
					userCell = row.DisplayName
				}
			}
			rec := []string{
				row.ClickedAt.UTC().Format(time.RFC3339),
				csvGuardCell(row.Referrer),
				csvGuardCell(uaCell),
				browser,
				osFamily,
				strconv.FormatBool(row.UserID != ""),
				csvGuardCell(userCell),
			}
			if err := cw.Write(rec); err != nil {
				return err
			}
		}
		if len(page) < batch {
			break
		}
		last := page[len(page)-1]
		q.AfterTS, q.AfterID = last.ClickedAt, last.ID
		remaining -= len(page)
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// csvGuardCell neutralizes spreadsheet formula injection per OWASP guidance:
// a cell whose first non-whitespace/control character is =, +, -, or @ —
// after skipping any run of leading whitespace or control characters (TAB,
// CR, LF, spaces) — is prefixed with a single quote so spreadsheet
// applications treat it as text; cells that begin with TAB or CR are likewise
// escaped. This is in addition to the RFC 4180 quoting encoding/csv applies.
// Referrers are attacker-controlled by construction (any site can send any
// Referer), so this is mandatory, not defense-in-depth.
// Governing: SPEC-0021 Security Requirements "CSV Injection" — Scenario Formula cell neutralized
func csvGuardCell(cell string) string {
	if cell == "" {
		return cell
	}
	// Cells beginning with TAB (0x09) or CR (0x0D) are escaped outright.
	if cell[0] == '\t' || cell[0] == '\r' {
		return "'" + cell
	}
	for _, r := range cell {
		if r == ' ' || r < 0x20 {
			continue // skip the leading whitespace/control run
		}
		switch r {
		case '=', '+', '-', '@':
			return "'" + cell
		}
		break
	}
	return cell
}

// EncodeClickExportCursor builds the opaque continuation cursor from a row's
// (clicked_at, id) keyset position. The cursor is a position, not a
// capability: every request presenting it is re-authorized via CanStats for
// the requested link.
// Governing: SPEC-0021 REQ "CSV Export"
func EncodeClickExportCursor(ts time.Time, id string) string {
	return base64.URLEncoding.EncodeToString([]byte(ts.UTC().Format(time.RFC3339Nano) + "\x00" + id))
}

// DecodeClickExportCursor reverses EncodeClickExportCursor. Malformed input
// is an error (the routes answer 400).
// Governing: SPEC-0021 REQ "CSV Export"
func DecodeClickExportCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor timestamp")
	}
	return ts, parts[1], nil
}
