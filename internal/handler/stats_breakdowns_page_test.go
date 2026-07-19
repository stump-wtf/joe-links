// Story #277 — breakdown panels on the stats page (SPEC-0021).
//
// Web-surface twins of the API scenario tests in internal/api/breakdowns_test.go:
// the panels render below the chart, follow the chart's 30/90-day window via
// an hx-swap-oob fragment, and carry no attribution for any viewer.
//
// Governing: SPEC-0021 REQ "Click Breakdowns", REQ "Capability Gating of Analytics Surfaces"
package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Scenario (web): Referrers grouped by host — the stats page shows the
// referrer table grouped by host with the Direct / unknown row, plus the
// browser/OS families and the auth split.
// Governing: SPEC-0021 REQ "Click Breakdowns"
func TestStatsPage_BreakdownPanelsRender(t *testing.T) {
	env := newStatsChartEnv(t)
	base := utcMidnightToday().Add(-2 * time.Hour) // safely inside the 30-day window
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0"
	seedExportClickAt(t, env.db, env.link.ID, "bdp-1", "https://a.example/x", ua, env.owner.ID, base)
	seedExportClickAt(t, env.db, env.link.ID, "bdp-2", "https://a.example/y?z=1", ua, "", base)
	seedExportClickAt(t, env.db, env.link.ID, "bdp-3", "", "curl/8.6.0", "", base)

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false)
	if w.Code != http.StatusOK {
		t.Fatalf("stats page status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `id="stats-breakdowns"`) {
		t.Fatalf("stats page missing the breakdowns panel; body=%s", body)
	}
	// Referrers grouped by host: path/query variants collapse to one host row.
	if !strings.Contains(body, "a.example") {
		t.Errorf("breakdowns missing referrer host a.example")
	}
	if !strings.Contains(body, "Direct / unknown") {
		t.Errorf("breakdowns missing the Direct / unknown row for the empty referrer")
	}
	// Browser and OS families parsed at read time.
	for _, want := range []string{"Firefox", "Windows", "Bot/CLI"} {
		if !strings.Contains(body, want) {
			t.Errorf("breakdowns missing family %q", want)
		}
	}
	// Auth split with counts and percentages.
	if !strings.Contains(body, "Authenticated") || !strings.Contains(body, "Anonymous") {
		t.Errorf("breakdowns missing the auth split rows")
	}
	if !strings.Contains(body, "33.3%") || !strings.Contains(body, "66.7%") {
		t.Errorf("breakdowns missing the auth percentages; body=%s", body)
	}
}

// Scenario (web): Recipient gets breakdowns without attribution — a share
// recipient's stats page renders all three breakdown panels and no clicker
// identity anywhere.
// Governing: SPEC-0021 REQ "Click Breakdowns", REQ "Capability Gating of Analytics Surfaces"
func TestStatsPage_RecipientGetsBreakdownsWithoutAttribution(t *testing.T) {
	env := newStatsChartEnv(t)
	base := utcMidnightToday().Add(-2 * time.Hour)
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15"
	seedExportClickAt(t, env.db, env.link.ID, "bdr-1", "https://news.ycombinator.com/item", ua, env.owner.ID, base)

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.recipient, false)
	if w.Code != http.StatusOK {
		t.Fatalf("recipient stats status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `id="stats-breakdowns"`) {
		t.Fatalf("recipient view missing the breakdowns panel")
	}
	for _, want := range []string{"news.ycombinator.com", "Safari", "macOS", "Authenticated"} {
		if !strings.Contains(body, want) {
			t.Errorf("recipient breakdowns missing %q", want)
		}
	}
	if strings.Contains(body, env.owner.DisplayName) {
		t.Errorf("recipient view must not name the clicker %q", env.owner.DisplayName)
	}
}

// The chart window toggle updates the breakdowns too: the HTMX chart fragment
// response carries a second copy of the breakdowns partial whose root swaps
// out-of-band, covering the same newly selected window.
// Governing: SPEC-0021 REQ "Click Breakdowns" — same selectable 30/90-day window as the series
func TestStatsChart_WindowToggleAlsoSwapsBreakdowns(t *testing.T) {
	env := newStatsChartEnv(t)
	seedExportClickAt(t, env.db, env.link.ID, "bdt-1", "https://a.example/x", "curl/8.6.0", "", utcMidnightToday().Add(-2*time.Hour))

	frag := env.get(t, "/dashboard/links/"+env.link.ID+"/stats/chart?days=90", env.owner, true)
	if frag.Code != http.StatusOK {
		t.Fatalf("chart fragment status = %d, want 200; body=%s", frag.Code, frag.Body.String())
	}
	body := frag.Body.String()
	if !strings.Contains(body, `id="daily-clicks-chart"`) {
		t.Errorf("fragment missing the primary chart swap target")
	}
	if !strings.Contains(body, `id="stats-breakdowns" hx-swap-oob="outerHTML"`) {
		t.Errorf("fragment missing the out-of-band breakdowns swap; body=%s", body)
	}
	if !strings.Contains(body, "last 90 days") {
		t.Errorf("out-of-band breakdowns must cover the toggled 90-day window; body=%s", body)
	}

	// The full-page render carries the panel without the OOB marker.
	page := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false)
	if page.Code != http.StatusOK {
		t.Fatalf("stats page status = %d", page.Code)
	}
	if strings.Contains(page.Body.String(), "hx-swap-oob") {
		t.Errorf("full page render must not mark the breakdowns as out-of-band")
	}
}
