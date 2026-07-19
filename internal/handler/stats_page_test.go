package handler

// Story #276 — stats-page daily-clicks chart (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "Chart renders with gap days"
//   - "Window toggle swaps fragment"
//   - "Share recipient sees the chart"
//   - "Pruned days distinguished from zero days" (template-level; the
//     JOE_CLICK_RETENTION configuration threading is pinned end-to-end in
//     stats_retention_test.go, story #278)
//
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

type statsChartEnv struct {
	router    http.Handler
	db        *sqlx.DB
	owner     *store.User
	recipient *store.User
	link      *store.Link
}

// newStatsChartEnv mirrors NewRouter's stats wiring with a secure link owned
// by owner and shared with recipient (whose only relationship to the link is
// the link_shares record).
func newStatsChartEnv(t *testing.T) *statsChartEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tagStore := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tagStore)
	us := store.NewUserStore(db)
	cs := store.NewClickStore(db)

	ctx := context.Background()
	owner, err := us.Upsert(ctx, "test", "chart-owner", "chart-owner@example.com", "Chart Owner", "user")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	recipient, err := us.Upsert(ctx, "test", "chart-recipient", "chart-recipient@example.com", "Chart Recipient", "user")
	if err != nil {
		t.Fatalf("seed recipient: %v", err)
	}
	link, err := ls.Create(ctx, "chart-link", "https://example.com/chart", owner.ID, "Chart", "", "secure")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	statsHandler := NewStatsHandler(ls, cs, owns, 0)
	r := chi.NewRouter()
	r.Get("/dashboard/links/{id}/stats", statsHandler.Show)
	r.Get("/dashboard/links/{id}/stats/chart", statsHandler.Chart)
	// Governing: SPEC-0021 REQ "CSV Export" — session-authenticated export route
	r.Get("/dashboard/links/{id}/stats/export", statsHandler.Export)

	return &statsChartEnv{router: r, db: db, owner: owner, recipient: recipient, link: link}
}

// get issues a GET as the given user; htmx toggles the HX-Request header.
func (e *statsChartEnv) get(t *testing.T, path string, user *store.User, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, user))
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// seedClickAt inserts a click row with an explicit clicked_at (RecordClick
// stamps time.Now(), so deterministic bucketing needs raw inserts).
func seedClickAt(t *testing.T, db *sqlx.DB, linkID string, ts time.Time, userID string) {
	t.Helper()
	var uid interface{}
	if userID != "" {
		uid = userID
	}
	_, err := db.ExecContext(context.Background(), db.Rebind(`
		INSERT INTO link_clicks (id, link_id, user_id, ip_hash, user_agent, referrer, clicked_at)
		VALUES (?, ?, ?, 'h', '', '', ?)
	`), uuid.New().String(), linkID, uid, ts)
	if err != nil {
		t.Fatalf("seed click at %s: %v", ts, err)
	}
}

// utcMidnightToday returns today's UTC day opening, the newest bucket of every
// chart window (SPEC-0021 pins the window boundary).
func utcMidnightToday() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Scenario: Chart renders with gap days — an owner opens the stats page for a
// link clicked on only 3 of the last 30 days; the SVG chart shows 30 day
// positions with zero-height marks on the 27 unclicked days.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestStatsPage_ChartRendersWithGapDays(t *testing.T) {
	env := newStatsChartEnv(t)
	base := utcMidnightToday()
	for _, daysAgo := range []int{2, 5, 9} {
		seedClickAt(t, env.db, env.link.ID, base.AddDate(0, 0, -daysAgo).Add(12*time.Hour), "")
	}

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<svg") {
		t.Fatalf("stats page missing inline SVG chart; body=%s", body)
	}
	if got := strings.Count(body, "data-date="); got != 30 {
		t.Errorf("chart day positions = %d, want 30", got)
	}
	if got := strings.Count(body, `data-count="0"`); got != 27 {
		t.Errorf("zero-count day positions = %d, want 27", got)
	}
	// Zero days render zero-height marks, clicked days render bars.
	if got := strings.Count(body, "chart-zero"); got != 27 {
		t.Errorf("zero-height marks = %d, want 27", got)
	}
	if got := strings.Count(body, "chart-bar"); got != 3 {
		t.Errorf("count bars = %d, want 3", got)
	}
}

// Scenario: Window toggle swaps fragment — activating the 90-day toggle
// issues an HTMX request refetching the chart partial with days=90, and the
// swapped fragment covers 90 gap-filled days.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestStatsPage_WindowToggleSwapsFragment(t *testing.T) {
	env := newStatsChartEnv(t)

	// The stats page wires the toggle to the chart fragment route.
	page := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false)
	if page.Code != http.StatusOK {
		t.Fatalf("stats page status = %d, want 200", page.Code)
	}
	pageBody := page.Body.String()
	if !strings.Contains(pageBody, `hx-get="/dashboard/links/`+env.link.ID+`/stats/chart?days=90"`) {
		t.Errorf("stats page missing 90-day HTMX toggle; body=%s", pageBody)
	}
	if !strings.Contains(pageBody, `hx-target="#daily-clicks-chart"`) {
		t.Errorf("toggle must target the chart fragment container")
	}

	// The HTMX request swaps a fragment covering 90 gap-filled days.
	frag := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/chart?days=90", env.owner, true)
	if frag.Code != http.StatusOK {
		t.Fatalf("chart fragment status = %d, want 200; body=%s", frag.Code, frag.Body.String())
	}
	fragBody := frag.Body.String()
	if strings.Contains(fragBody, "<html") {
		t.Errorf("chart response must be a fragment, not a full page")
	}
	if !strings.Contains(fragBody, `id="daily-clicks-chart"`) {
		t.Errorf("fragment missing swap container id; body=%s", fragBody)
	}
	if got := strings.Count(fragBody, "data-date="); got != 90 {
		t.Errorf("90-day fragment day positions = %d, want 90", got)
	}
}

// Web-UI fallback: days values other than 30 and 90 fall back to 30 (the API
// twin rejects them with 400 instead).
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestStatsPage_InvalidDaysFallsBackTo30(t *testing.T) {
	env := newStatsChartEnv(t)

	for _, path := range []string{
		"/dashboard/links/" + env.link.ID + "/stats?days=7",
		"/dashboard/links/" + env.link.ID + "/stats/chart?days=7",
	} {
		w := env.get(t, path, env.owner, true)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, w.Code)
		}
		if got := strings.Count(w.Body.String(), "data-date="); got != 30 {
			t.Errorf("%s day positions = %d, want 30 (web UI falls back)", path, got)
		}
	}
}

// Scenario: Share recipient sees the chart — a user whose only relationship
// to a secure link is a link_shares record opens the stats page and the daily
// chart renders, counts only, with no user attribution anywhere.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", REQ "Capability Gating of Analytics Surfaces"
func TestStatsPage_ShareRecipientSeesChart(t *testing.T) {
	env := newStatsChartEnv(t)
	// An authenticated click by the owner: attribution exists in the rows but
	// must not surface for the recipient.
	seedClickAt(t, env.db, env.link.ID, utcMidnightToday().AddDate(0, 0, -1).Add(12*time.Hour), env.owner.ID)

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.recipient, false)
	if w.Code != http.StatusOK {
		t.Fatalf("recipient stats status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "data-count=") {
		t.Fatalf("recipient must see the daily chart; body=%s", body)
	}
	// Counts only: no clicker identity anywhere (PR #255 rule).
	if strings.Contains(body, "<th>User</th>") {
		t.Errorf("recipient view must not render the clicker attribution column")
	}
	if strings.Contains(body, env.owner.DisplayName) {
		t.Errorf("recipient view must not name the clicker %q", env.owner.DisplayName)
	}
}

// The chart fragment route carries the same CanStats gate as the enclosing
// stats page ("one matrix, three surfaces agree"): an unrelated authenticated
// user gets 403 from the fragment route directly, while the share recipient
// gets the fragment. Pins the route-level gate independently of the page.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", REQ "Capability Gating of Analytics Surfaces"
func TestStatsChart_UnrelatedUserForbidden(t *testing.T) {
	env := newStatsChartEnv(t)
	us := store.NewUserStore(env.db)
	stranger, err := us.Upsert(context.Background(), "test", "chart-stranger", "chart-stranger@example.com", "Chart Stranger", "user")
	if err != nil {
		t.Fatalf("seed stranger: %v", err)
	}

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/chart?days=90", stranger, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("stranger chart fragment status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "data-date=") {
		t.Errorf("403 response must not leak chart data")
	}

	// The share recipient passes the same gate on the fragment route itself.
	rec := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/chart?days=90", env.recipient, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("recipient chart fragment status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.Count(rec.Body.String(), "data-date="); got != 90 {
		t.Errorf("recipient fragment day positions = %d, want 90", got)
	}
}

// A direct (non-HTMX) browser hit on the toggle URL must not return a bare
// fragment: it redirects to the enclosing stats page with the same window so
// the URL stays shareable.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestStatsChart_NonHTMXRedirectsToStatsPage(t *testing.T) {
	env := newStatsChartEnv(t)

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/chart?days=90", env.owner, false)
	if w.Code != http.StatusFound {
		t.Fatalf("non-HTMX chart status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("Location"), "/dashboard/links/"+env.link.ID+"/stats?days=90"; got != want {
		t.Errorf("redirect Location = %q, want %q", got, want)
	}
}

// Scenario: Pruned days distinguished from zero days — with a 60-day horizon
// and the 90-day window, the oldest 30 day positions render as no-data bands,
// visually distinct from zero-count days inside the horizon. Template-level:
// the rendering contract is pinned against the partial directly; the
// JOE_CLICK_RETENTION handler threading is pinned end-to-end in
// stats_retention_test.go (story #278). Also pins the deliberate
// partial-bucket decision: the bucket containing the horizon (Pruned with
// Count > 0) renders BOTH the no-data band and a lower-bound bar.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", REQ "Click Retention"
func TestStatsChart_PrunedDaysDistinguishedFromZeroDays(t *testing.T) {
	start := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	series := make([]store.DailyClickCount, 0, 90)
	for i := 0; i < 90; i++ {
		d := store.DailyClickCount{Date: start.AddDate(0, 0, i).Format("2006-01-02")}
		if i < 30 {
			d.Pruned = true
		}
		if i == 29 {
			d.Count = 2 // horizon-crossing bucket: pruned day with surviving rows
		}
		if i == 40 {
			d.Count = 5
		}
		series = append(series, d)
	}

	data := StatsChartData{LinkID: "test-link", Days: 90, Chart: buildClickChart(series, 90)}
	rr := httptest.NewRecorder()
	renderFragment(rr, "stats_chart", data)
	body := rr.Body.String()
	if strings.Contains(body, "template error") {
		t.Fatalf("chart partial crashed: %s", body)
	}

	if got := strings.Count(body, `data-pruned="true"`); got != 30 {
		t.Errorf("pruned day positions = %d, want 30", got)
	}
	// Every pruned day renders the no-data band; no pruned day renders a
	// zero-height mark (a pruned day is not an unclicked day).
	if got := strings.Count(body, "chart-nodata"); got != 30 {
		t.Errorf("no-data bands = %d, want 30", got)
	}
	if got := strings.Count(body, "chart-zero"); got != 59 {
		t.Errorf("zero-height marks = %d, want 59 (in-horizon zero days only)", got)
	}
	if !strings.Contains(body, "no data (before the retention horizon)") {
		t.Errorf("pruned days must be tooltipped as no-data, not zero")
	}

	// The horizon-crossing bucket (2026-05-19) carries the band AND a bar,
	// tooltipped as a lower bound.
	partial := regexp.MustCompile(`(?s)<g data-date="2026-05-19" data-count="2" data-pruned="true">.*?chart-nodata.*?chart-bar.*?</g>`)
	if !partial.MatchString(body) {
		t.Errorf("horizon-crossing bucket must render both the no-data band and an observed-count bar; body=%s", body)
	}
	if !strings.Contains(body, "at least 2 clicks") {
		t.Errorf("horizon-crossing bucket tooltip must state a lower bound")
	}

	// Retention legend shows only when pruned days are present.
	if !strings.Contains(body, "retention horizon") {
		t.Errorf("chart with pruned days must carry the retention legend")
	}
}

// buildClickChart geometry sanity: one bar per day, ascending x positions,
// max count scales to the full plot height, zero days get baseline ticks.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
func TestBuildClickChart_GeometryAndScale(t *testing.T) {
	start := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	series := make([]store.DailyClickCount, 0, 30)
	for i := 0; i < 30; i++ {
		d := store.DailyClickCount{Date: start.AddDate(0, 0, i).Format("2006-01-02")}
		if i == 10 {
			d.Count = 8 // max
		}
		if i == 20 {
			d.Count = 4
		}
		series = append(series, d)
	}

	c := buildClickChart(series, 30)
	if len(c.Bars) != 30 {
		t.Fatalf("bars = %d, want 30", len(c.Bars))
	}
	if c.MaxCount != 8 || c.Total != 12 {
		t.Errorf("MaxCount=%d Total=%d, want 8 and 12", c.MaxCount, c.Total)
	}
	for i := 1; i < len(c.Bars); i++ {
		if c.Bars[i].X <= c.Bars[i-1].X {
			t.Fatalf("bar x positions must ascend: bar[%d].X=%v <= bar[%d].X=%v", i, c.Bars[i].X, i-1, c.Bars[i-1].X)
		}
	}
	if c.Bars[10].Height != c.PlotHeight {
		t.Errorf("max-count bar height = %v, want full plot height %v", c.Bars[10].Height, c.PlotHeight)
	}
	if c.Bars[20].Height >= c.Bars[10].Height || c.Bars[20].Height <= 0 {
		t.Errorf("mid-count bar height = %v, want between 0 and %v", c.Bars[20].Height, c.Bars[10].Height)
	}
	if c.Bars[0].Height != chartZeroTickH {
		t.Errorf("zero-day tick height = %v, want %v", c.Bars[0].Height, chartZeroTickH)
	}
}
