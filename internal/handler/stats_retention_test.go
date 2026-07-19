package handler

// Story #278 — retention wiring on the per-link stats page (SPEC-0021).
//
// Tests are named after the spec scenarios they implement:
//   - "Labeled totals under retention"
//
// plus the configuration-threading half of "Pruned days distinguished from
// zero days": the rendering contract was pinned template-level with story
// #276 (stats_page_test.go); here the real JOE_CLICK_RETENTION value flows
// through the handler, replacing the retentionDays=0 placeholder.
//
// Governing: SPEC-0021 REQ "Click Retention", REQ "Per-Link Daily Time Series", ADR-0021

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/store"
)

// withRetention swaps the env's router for one whose stats handler carries
// the given retention horizon, over the same database. Tests toggle between
// horizons on one env because envs within a test share the in-memory DB.
func (e *statsChartEnv) withRetention(t *testing.T, retentionDays int) *statsChartEnv {
	t.Helper()
	owns := store.NewOwnershipStore(e.db)
	tagStore := store.NewTagStore(e.db)
	ls := store.NewLinkStore(e.db, owns, tagStore)
	cs := store.NewClickStore(e.db)

	statsHandler := NewStatsHandler(ls, cs, owns, retentionDays)
	r := chi.NewRouter()
	r.Get("/dashboard/links/{id}/stats", statsHandler.Show)
	r.Get("/dashboard/links/{id}/stats/chart", statsHandler.Chart)
	e.router = r
	return e
}

// Scenario: Labeled totals under retention — with retention active, the
// stats-page total is labeled as covering the retention window, not "all
// time".
// Governing: SPEC-0021 REQ "Click Retention"
func TestStatsPage_LabeledTotalsUnderRetention(t *testing.T) {
	env := newStatsChartEnv(t).withRetention(t, 365)

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "All Time") {
		t.Errorf("retention-active stats page must not label the total 'All Time'")
	}
	if !strings.Contains(body, "Since ") || !strings.Contains(body, "365-day retention window") {
		t.Errorf("total must be labeled as covering the retention window; body=%s", body)
	}

	// And with retention off (the default), the v1 label is untouched.
	offBody := env.withRetention(t, 0).get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false).Body.String()
	if !strings.Contains(offBody, "All Time") {
		t.Errorf("retention-off stats page keeps the all-time label; body=%s", offBody)
	}
}

// Configuration threading for "Pruned days distinguished from zero days":
// with a 60-day horizon wired through the handler (no longer the
// retentionDays=0 placeholder), the 90-day chart renders its oldest 30 day
// positions as pruned no-data bands end-to-end.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", REQ "Click Retention"
func TestStatsPage_PrunedDaysWiredFromRetentionConfig(t *testing.T) {
	env := newStatsChartEnv(t).withRetention(t, 60)

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats?days=90", env.owner, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if got := strings.Count(body, `data-pruned="true"`); got != 30 {
		t.Errorf("pruned day positions = %d, want 30 (60-day horizon over the 90-day window)", got)
	}

	// Retention off leaves every day unpruned — the placeholder behavior is
	// now the configured-off behavior, not a hardcode.
	offBody := env.withRetention(t, 0).get(t, "/dashboard/links/"+env.link.ID+"/stats?days=90", env.owner, false).Body.String()
	if strings.Contains(offBody, `data-pruned="true"`) {
		t.Errorf("retention-off chart must not mark any day pruned")
	}
}
