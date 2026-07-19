// Story #277 — CSV export via the session-authenticated dashboard route
// (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "Owner exports via UI button"
//   - "Recipient export has empty attribution columns" (web-route twin of the
//     API-route test in internal/api/export_test.go)
//   - "Cap and resume across a timestamp tie"
//
// Governing: SPEC-0021 REQ "CSV Export", REQ "Capability Gating of Analytics Surfaces"
package handler

import (
	"context"
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/store"
)

// exportCSVHeader is the exact header row and column order SPEC-0021 pins.
var exportCSVHeader = []string{"clicked_at", "referrer", "user_agent", "browser", "os", "authenticated", "user"}

// seedExportClickAt inserts a click row with explicit referrer, user agent,
// user, id, and clicked_at so keyset ordering and attribution are
// deterministic.
func seedExportClickAt(t *testing.T, db *sqlx.DB, linkID, id, referrer, ua, userID string, ts time.Time) {
	t.Helper()
	var uid interface{}
	if userID != "" {
		uid = userID
	}
	_, err := db.ExecContext(context.Background(), db.Rebind(`
		INSERT INTO link_clicks (id, link_id, user_id, ip_hash, user_agent, referrer, clicked_at)
		VALUES (?, ?, ?, 'h', ?, ?, ?)
	`), id, linkID, uid, ua, referrer, ts)
	if err != nil {
		t.Fatalf("seed click %s: %v", id, err)
	}
}

// parseExportCSV decodes a response body into records.
func parseExportCSV(t *testing.T, body string) [][]string {
	t.Helper()
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v; body: %s", err, body)
	}
	return records
}

// Scenario: Owner exports via UI button — the stats page carries an "Export
// CSV" button backed by the session route, and clicking it downloads a
// streamed CSV with the exact header, rows oldest-first, and populated user
// and user_agent cells for authenticated clicks.
// Governing: SPEC-0021 REQ "CSV Export"
func TestCSVExport_OwnerExportsViaUIButton(t *testing.T) {
	env := newStatsChartEnv(t)

	// The stats page renders the button pointing at the session export route.
	page := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false)
	if page.Code != http.StatusOK {
		t.Fatalf("stats page status = %d, want 200", page.Code)
	}
	if !strings.Contains(page.Body.String(), `href="/dashboard/links/`+env.link.ID+`/stats/export"`) {
		t.Errorf("stats page missing the Export CSV button; body=%s", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "Export CSV") {
		t.Errorf("stats page missing the Export CSV label")
	}

	base := time.Date(2026, 7, 5, 14, 0, 0, 0, time.UTC)
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0"
	seedExportClickAt(t, env.db, env.link.ID, "exp-b", "https://ref.example/b", ua, env.owner.ID, base.Add(time.Hour))
	seedExportClickAt(t, env.db, env.link.ID, "exp-a", "https://ref.example/a", "curl/8.6.0", "", base)

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/export", env.owner, false)
	if w.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="`+env.link.Slug+`-clicks.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}

	records := parseExportCSV(t, w.Body.String())
	if len(records) != 3 {
		t.Fatalf("records = %d, want header + 2 rows", len(records))
	}
	for i, col := range exportCSVHeader {
		if records[0][i] != col {
			t.Fatalf("header = %v, want %v", records[0], exportCSVHeader)
		}
	}
	// Oldest first.
	if records[1][0] != base.Format(time.RFC3339) {
		t.Errorf("first row clicked_at = %q, want %q (oldest first)", records[1][0], base.Format(time.RFC3339))
	}
	// The authenticated click carries raw UA and the clicker's display name
	// for the owner (a CanManageShares caller).
	if got := records[2]; got[2] != ua || got[5] != "true" || got[6] != env.owner.DisplayName {
		t.Errorf("authenticated row = %v, want raw UA and user %q populated", got, env.owner.DisplayName)
	}
}

// Scenario: Recipient export has empty attribution columns — the session
// route serves a share recipient the same seven columns with every user and
// user_agent cell empty while browser/os stay populated.
// Governing: SPEC-0021 REQ "CSV Export", Security Requirements "Clicker Attribution and the Share Roster"
func TestCSVExport_RecipientExportHasEmptyAttributionColumns(t *testing.T) {
	env := newStatsChartEnv(t)
	base := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	ua := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1"
	seedExportClickAt(t, env.db, env.link.ID, "exp-r-1", "https://ref.example/x", ua, env.owner.ID, base)

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/export", env.recipient, false)
	if w.Code != http.StatusOK {
		t.Fatalf("recipient export status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	records := parseExportCSV(t, w.Body.String())
	if len(records) != 2 {
		t.Fatalf("records = %d, want header + 1 row", len(records))
	}
	for i, col := range exportCSVHeader {
		if records[0][i] != col {
			t.Fatalf("recipient header = %v, want the same seven columns %v", records[0], exportCSVHeader)
		}
	}
	row := records[1]
	if row[2] != "" || row[6] != "" {
		t.Errorf("recipient row = %v, want empty user_agent and user cells", row)
	}
	if row[3] != "Safari" || row[4] != "iOS" {
		t.Errorf("recipient browser/os = %q/%q, want Safari/iOS (still populated)", row[3], row[4])
	}
	if strings.Contains(w.Body.String(), env.owner.DisplayName) {
		t.Errorf("recipient export leaks the clicker display name")
	}
}

// Scenario: Cap and resume across a timestamp tie — when the row cap
// truncates the export mid-tie, exactly cap rows stream (oldest first) and
// X-Next-Cursor carries an opaque (clicked_at, id) cursor; the follow-up
// request returns the remaining rows with no tied row skipped and none
// duplicated, and carries no X-Next-Cursor. Runs with the cap lowered to 10
// (the cap is a var for exactly this test; production uses
// store.ExportRowCap = 100000).
// Governing: SPEC-0021 REQ "CSV Export"
func TestCSVExport_CapAndResumeAcrossATimestampTie(t *testing.T) {
	oldCap := exportRowCap
	exportRowCap = 10
	t.Cleanup(func() { exportRowCap = oldCap })

	env := newStatsChartEnv(t)
	base := time.Date(2026, 7, 7, 8, 0, 0, 0, time.UTC)

	// 14 rows: 8 with distinct ascending timestamps, then 6 sharing one
	// clicked_at (rows 9–14) so the cap boundary at row 10 falls inside the
	// tie — the case a bare-timestamp cursor would skip or duplicate.
	tie := base.Add(9 * time.Minute)
	for i := 1; i <= 8; i++ {
		id := "tie-" + string(rune('a'+i-1)) // tie-a … tie-h
		seedExportClickAt(t, env.db, env.link.ID, id, "", "", "", base.Add(time.Duration(i)*time.Minute))
	}
	for i := 1; i <= 6; i++ {
		id := "tie-z" + string(rune('0'+i)) // tie-z1 … tie-z6, same clicked_at
		seedExportClickAt(t, env.db, env.link.ID, id, "", "", "", tie)
	}

	// First request: exactly 10 rows + a continuation cursor.
	first := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/export", env.owner, false)
	if first.Code != http.StatusOK {
		t.Fatalf("first export status = %d; body=%s", first.Code, first.Body.String())
	}
	firstRecords := parseExportCSV(t, first.Body.String())
	if len(firstRecords)-1 != 10 {
		t.Fatalf("first response rows = %d, want exactly 10 (the cap)", len(firstRecords)-1)
	}
	cursor := first.Header().Get("X-Next-Cursor")
	if cursor == "" {
		t.Fatalf("first response missing X-Next-Cursor despite truncation")
	}

	// Second request resumes exclusively after the cap row.
	second := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/export?cursor="+cursor, env.owner, false)
	if second.Code != http.StatusOK {
		t.Fatalf("second export status = %d; body=%s", second.Code, second.Body.String())
	}
	secondRecords := parseExportCSV(t, second.Body.String())
	if len(secondRecords)-1 != 4 {
		t.Fatalf("second response rows = %d, want the remaining 4", len(secondRecords)-1)
	}
	if next := second.Header().Get("X-Next-Cursor"); next != "" {
		t.Errorf("second response X-Next-Cursor = %q, want absent (export complete)", next)
	}

	// No row skipped, none duplicated: 14 distinct timestamps stitched across
	// both responses, in keyset order.
	var got []string
	for _, rec := range append(firstRecords[1:], secondRecords[1:]...) {
		got = append(got, rec[0])
	}
	if len(got) != 14 {
		t.Fatalf("total rows across responses = %d, want 14", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("rows out of keyset order at %d: %q < %q", i, got[i], got[i-1])
		}
	}
	tied := 0
	for _, ts := range got {
		if ts == tie.Format(time.RFC3339) {
			tied++
		}
	}
	if tied != 6 {
		t.Errorf("tied-timestamp rows across responses = %d, want all 6 (no skip, no duplicate)", tied)
	}
}

// The session route carries the same CanStats gate as the stats page: an
// unrelated authenticated user gets the styled 403 and no CSV; anonymous
// users are redirected to login.
// Governing: SPEC-0021 REQ "CSV Export", REQ "Capability Gating of Analytics Surfaces"
func TestCSVExport_WebRouteAuthz(t *testing.T) {
	env := newStatsChartEnv(t)
	us := store.NewUserStore(env.db)
	stranger, err := us.Upsert(context.Background(), "test", "export-stranger", "export-stranger@example.com", "Export Stranger", "user")
	if err != nil {
		t.Fatalf("seed stranger: %v", err)
	}

	if w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/export", stranger, false); w.Code != http.StatusForbidden {
		t.Errorf("stranger export status = %d, want 403", w.Code)
	}
	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/export", nil, false)
	if w.Code != http.StatusFound {
		t.Errorf("anonymous export status = %d, want 302 redirect to login", w.Code)
	} else if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/auth/login") {
		t.Errorf("anonymous export redirects to %q, want /auth/login", loc)
	}

	// Invalid window parameters are 400 on the web route too.
	if w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/export?from=yesterday", env.owner, false); w.Code != http.StatusBadRequest {
		t.Errorf("invalid from status = %d, want 400", w.Code)
	}
	if w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/export?cursor=!!!", env.owner, false); w.Code != http.StatusBadRequest {
		t.Errorf("malformed cursor status = %d, want 400", w.Code)
	}
}
