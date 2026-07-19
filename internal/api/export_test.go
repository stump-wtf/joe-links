// Story #277 — CSV export API route (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "Recipient export has empty attribution columns" (API route; the web
//     route twin lives in internal/handler)
//   - "Formula cell neutralized"
//   - "API route refuses session auth"
//
// The "Owner exports via UI button" and "Cap and resume across a timestamp
// tie" scenarios are exercised against the session route in
// internal/handler/stats_export_test.go — both routes share one store
// iterator and one encoder (ADR-0021).
//
// Governing: SPEC-0021 REQ "CSV Export", REQ "Capability Gating of Analytics Surfaces"
package api_test

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/store"
)

// exportCSVHeader is the exact header row and column order SPEC-0021 pins.
var exportCSVHeader = []string{"clicked_at", "referrer", "user_agent", "browser", "os", "authenticated", "user"}

// getExport performs a GET on the API export route and returns the recorder.
func getExport(t *testing.T, env *testEnv, linkID, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/links/"+linkID+"/stats/export"+query, nil)
	if token != "" {
		authRequest(req, token)
	}
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)
	return rec
}

// parseCSV decodes a response body into records.
func parseCSV(t *testing.T, body string) [][]string {
	t.Helper()
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v; body: %s", err, body)
	}
	return records
}

// Owner export over the API route: exact header, oldest-first rows, populated
// user and user_agent cells for authenticated clicks, and no X-Next-Cursor
// when the export is complete.
// Governing: SPEC-0021 REQ "CSV Export"
func TestCSVExport_OwnerExportPopulatedAttribution(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "csv-owner@example.com", "user")
	token := seedToken(t, env, owner.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "csv-owner-link", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0"
	seedBreakdownClick(t, env, link.ID, "csv-o-2", "https://ref.example/b", ua, owner.ID, base.Add(time.Hour))
	seedBreakdownClick(t, env, link.ID, "csv-o-1", "https://ref.example/a", "curl/8.6.0", "", base)

	rec := getExport(t, env, link.ID, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="csv-owner-link-clicks.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if next := rec.Header().Get("X-Next-Cursor"); next != "" {
		t.Errorf("X-Next-Cursor = %q, want absent (export complete)", next)
	}

	records := parseCSV(t, rec.Body.String())
	if len(records) != 3 {
		t.Fatalf("records = %d, want header + 2 rows", len(records))
	}
	for i, col := range exportCSVHeader {
		if records[0][i] != col {
			t.Fatalf("header = %v, want %v", records[0], exportCSVHeader)
		}
	}

	// Rows oldest-first by (clicked_at, id).
	if records[1][0] != base.Format(time.RFC3339) || records[2][0] != base.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("rows not oldest-first: %v / %v", records[1], records[2])
	}

	// Anonymous curl click: no user, browser/os families populated.
	if got := records[1]; got[1] != "https://ref.example/a" || got[2] != "curl/8.6.0" ||
		got[3] != "Bot/CLI" || got[4] != "Other" || got[5] != "false" || got[6] != "" {
		t.Errorf("anonymous row = %v", got)
	}
	// Authenticated Firefox click: raw UA and user display name populated for
	// the owner (a CanManageShares caller).
	if got := records[2]; got[2] != ua || got[3] != "Firefox" || got[4] != "Windows" ||
		got[5] != "true" || got[6] != owner.DisplayName {
		t.Errorf("authenticated row = %v (want ua %q, user %q)", got, ua, owner.DisplayName)
	}
}

// Scenario: Recipient export has empty attribution columns — a share
// recipient exporting the link gets the same seven columns, every user and
// user_agent cell empty, and browser/os cells still populated.
// Governing: SPEC-0021 REQ "CSV Export", Security Requirements "Clicker Attribution and the Share Roster"
func TestCSVExport_RecipientExportHasEmptyAttributionColumns(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "csv-share-owner@example.com", "user")
	recipient := seedUser(t, env, "csv-share-recipient@example.com", "user")
	recipientToken := seedToken(t, env, recipient.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "csv-shared", "https://example.com", owner.ID, "Shared", "", "secure")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := env.LinkStore.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	base := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15"
	seedBreakdownClick(t, env, link.ID, "csv-s-1", "https://ref.example/x", ua, owner.ID, base)
	seedBreakdownClick(t, env, link.ID, "csv-s-2", "", ua, owner.ID, base.Add(time.Minute))

	rec := getExport(t, env, link.ID, recipientToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("recipient status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	records := parseCSV(t, rec.Body.String())
	if len(records) != 3 {
		t.Fatalf("records = %d, want header + 2 rows", len(records))
	}
	for i, col := range exportCSVHeader {
		if records[0][i] != col {
			t.Fatalf("recipient header = %v, want the same seven columns %v", records[0], exportCSVHeader)
		}
	}
	for i, row := range records[1:] {
		if row[2] != "" {
			t.Errorf("row %d: user_agent cell = %q, want empty for recipients", i, row[2])
		}
		if row[6] != "" {
			t.Errorf("row %d: user cell = %q, want empty for recipients", i, row[6])
		}
		if row[3] != "Safari" || row[4] != "macOS" {
			t.Errorf("row %d: browser/os = %q/%q, want Safari/macOS (still populated)", i, row[3], row[4])
		}
		if row[5] != "true" {
			t.Errorf("row %d: authenticated = %q, want true", i, row[5])
		}
	}
	if strings.Contains(rec.Body.String(), owner.DisplayName) {
		t.Errorf("recipient export leaks the clicker display name")
	}
}

// Scenario: Formula cell neutralized — a stored referrer or UA value whose
// first non-whitespace/control character is =, +, -, or @ is emitted with a
// leading single quote so spreadsheet applications treat it as text.
// Governing: SPEC-0021 REQ "CSV Export", Security Requirements "CSV Injection"
func TestCSVExport_FormulaCellNeutralized(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "csv-formula@example.com", "user")
	token := seedToken(t, env, owner.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "csv-formula", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	base := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		id       string
		referrer string
		ua       string
		wantRef  string
		wantUA   string
	}{
		{"csv-f-1", "=SUM(A1:A9)", "=evil()", "'=SUM(A1:A9)", "'=evil()"},
		{"csv-f-2", "+plus", "-dash", "'+plus", "'-dash"},
		{"csv-f-3", "@at", "  =spaced", "'@at", "'  =spaced"},
		{"csv-f-4", "\t=tab-lead", "\ttab-start", "'\t=tab-lead", "'\ttab-start"},
		{"csv-f-5", "https://safe.example/x", "curl/8.6.0", "https://safe.example/x", "curl/8.6.0"},
	}
	for i, tc := range cases {
		seedBreakdownClick(t, env, link.ID, tc.id, tc.referrer, tc.ua, "", base.Add(time.Duration(i)*time.Minute))
	}

	rec := getExport(t, env, link.ID, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	records := parseCSV(t, rec.Body.String())
	if len(records) != len(cases)+1 {
		t.Fatalf("records = %d, want header + %d rows", len(records), len(cases))
	}
	for i, tc := range cases {
		row := records[i+1]
		if row[1] != tc.wantRef {
			t.Errorf("%s: referrer cell = %q, want %q", tc.id, row[1], tc.wantRef)
		}
		if row[2] != tc.wantUA {
			t.Errorf("%s: user_agent cell = %q, want %q", tc.id, row[2], tc.wantUA)
		}
	}
}

// Scenario: API route refuses session auth — a browser with a valid session
// cookie but no bearer token gets 401 from the /api/v1 export route
// (SPEC-0006: no session cookies on /api/v1; the paired /dashboard route
// exists for exactly this reason).
// Governing: SPEC-0021 REQ "CSV Export"
func TestCSVExport_APIRouteRefusesSessionAuth(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "csv-session@example.com", "user")

	link, err := env.LinkStore.Create(context.Background(), "csv-session", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	req := httptest.NewRequest("GET", "/links/"+link.ID+"/stats/export", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "valid-looking-session"})
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (session cookies rejected on /api/v1)", rec.Code)
	}
}

// Window bounds and cursor validation: invalid from/to and malformed cursors
// are 400; a valid cursor resumes exclusively after the cursor row; from/to
// bound the window inclusively.
// Governing: SPEC-0021 REQ "CSV Export"
func TestCSVExport_WindowAndCursorValidation(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "csv-window@example.com", "user")
	token := seedToken(t, env, owner.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "csv-window", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	base := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"csv-w-1", "csv-w-2", "csv-w-3"} {
		seedBreakdownClick(t, env, link.ID, id, "", "", "", base.Add(time.Duration(i)*time.Hour))
	}

	// Invalid from/to/cursor → 400.
	for _, query := range []string{"?from=yesterday", "?to=2026-13-99", "?cursor=%2Fnot-base64%2F", "?cursor=aGVsbG8="} {
		rec := getExport(t, env, link.ID, token, query)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body: %s", query, rec.Code, rec.Body.String())
		}
	}

	// from bounds the window: only the two newer rows.
	rec := getExport(t, env, link.ID, token, "?from="+base.Add(time.Hour).UTC().Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("from-window status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(parseCSV(t, rec.Body.String())) - 1; got != 2 {
		t.Errorf("from-window rows = %d, want 2", got)
	}

	// A cursor at the first row's keyset position resumes strictly after it.
	cursor := store.EncodeClickExportCursor(base, "csv-w-1")
	rec = getExport(t, env, link.ID, token, "?cursor="+cursor)
	if rec.Code != http.StatusOK {
		t.Fatalf("cursor status = %d; body: %s", rec.Code, rec.Body.String())
	}
	records := parseCSV(t, rec.Body.String())
	if len(records) != 3 {
		t.Fatalf("cursor rows = %d, want 2 (exclusive of the cursor row)", len(records)-1)
	}
	if records[1][0] != base.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("cursor resume starts at %q, want %q", records[1][0], base.Add(time.Hour).Format(time.RFC3339))
	}
}

// The export route enforces the same CanStats matrix as every stats surface:
// stranger 403, unknown link 404, unauthenticated 401.
// Governing: SPEC-0021 REQ "CSV Export", REQ "Capability Gating of Analytics Surfaces"
func TestCSVExport_StrangerForbidden(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "csv-authz-owner@example.com", "user")
	stranger := seedUser(t, env, "csv-authz-stranger@example.com", "user")
	strangerToken := seedToken(t, env, stranger.ID)

	link, err := env.LinkStore.Create(context.Background(), "csv-authz", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	if rec := getExport(t, env, link.ID, strangerToken, ""); rec.Code != http.StatusForbidden {
		t.Errorf("stranger status = %d, want 403", rec.Code)
	}
	if rec := getExport(t, env, "nonexistent-id", strangerToken, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown link status = %d, want 404", rec.Code)
	}
	if rec := getExport(t, env, link.ID, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
}
