// Story #278 — capability gating of analytics surfaces (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "One matrix, three surfaces agree"
//   - "Attribution never widens"
//
// (The web stats page's 403 for the same caller is pinned in
// internal/handler — shared_authz_test.go and forbidden_test.go; these tests
// pin the API half of the matrix plus the dashboard-contribution clause.)
//
// Governing: SPEC-0021 REQ "Capability Gating of Analytics Surfaces", ADR-0021
package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Scenario: One matrix, three surfaces agree — when a user's CanStats is
// false for a link, every per-link analytics API endpoint returns 403 and the
// link contributes nothing to that user's dashboard aggregates.
// Governing: SPEC-0021 REQ "Capability Gating of Analytics Surfaces"
func TestCapabilityGating_OneMatrixThreeSurfacesAgree(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "gate-owner@example.com", "user")
	stranger := seedUser(t, env, "gate-stranger@example.com", "user")
	strangerToken := seedToken(t, env, stranger.ID)
	ctx := context.Background()

	// A public link: the visibility most tempting to include, and exactly the
	// one CanStats still refuses to a stranger (public = resolvable and
	// browsable, not stats-readable).
	link, err := env.LinkStore.Create(ctx, "gate-public", "https://example.com/gate", owner.ID, "Gate", "", "public")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	seedAnalyticsClicks(t, env, link.ID, 6, analyticsThisWeek(t), "https://busy.example/page")

	// Every per-link analytics endpoint resolves the same LinkCaps matrix:
	// all of them refuse the stranger with 403.
	for _, path := range []string{
		"/links/" + link.ID + "/stats",
		"/links/" + link.ID + "/clicks",
		"/links/" + link.ID + "/stats/timeseries",
		"/links/" + link.ID + "/stats/breakdowns",
		"/links/" + link.ID + "/stats/export",
	} {
		req := httptest.NewRequest("GET", path, nil)
		authRequest(req, strangerToken)
		rec := httptest.NewRecorder()
		env.Router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403 (one matrix, all surfaces agree)", path, rec.Code)
		}
	}

	// And the link contributes nothing to the stranger's dashboard: not to
	// top links, not to referrers, not to never-clicked.
	body := decodeAnalytics(t, getAnalytics(t, env, strangerToken, ""))
	if len(body.TopLinks) != 0 || len(body.TopReferrers) != 0 || len(body.NeverClicked) != 0 {
		t.Errorf("a CanStats=false link must contribute nothing to the dashboard; got %+v", body)
	}
	raw := getAnalytics(t, env, strangerToken, "").Body.String()
	if strings.Contains(raw, "gate-public") || strings.Contains(raw, link.ID) {
		t.Errorf("analytics response must not mention the inaccessible link; body=%s", raw)
	}
}

// Scenario: Attribution never widens — every surface introduced by SPEC-0021,
// rendered for a caller without CanManageShares (a share recipient), carries
// no user ID, display name, or other clicker identity anywhere.
// Governing: SPEC-0021 REQ "Capability Gating of Analytics Surfaces"
func TestCapabilityGating_AttributionNeverWidens(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "gate-attr-owner@example.com", "user")
	recipient := seedUser(t, env, "gate-attr-recipient@example.com", "user")
	recipientToken := seedToken(t, env, recipient.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "gate-attr", "https://example.com/attr", owner.ID, "Attr", "", "secure")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := env.LinkStore.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}
	// Authenticated clicks by the owner: the rows carry attribution that must
	// never surface for the recipient on any SPEC-0021 surface.
	ts := analyticsThisWeek(t)
	for i := 0; i < 2; i++ {
		seedBreakdownClick(t, env, link.ID, fmt.Sprintf("gate-attr-%d", i),
			"https://ref.example/r", "Mozilla/5.0 (Windows NT 10.0; rv:141.0) Gecko/20100101 Firefox/141.0", owner.ID, ts)
	}

	surfaces := []string{
		"/links/" + link.ID + "/stats/timeseries",
		"/links/" + link.ID + "/stats/breakdowns",
		"/links/" + link.ID + "/stats/export",
		"/analytics",
	}
	for _, path := range surfaces {
		req := httptest.NewRequest("GET", path, nil)
		authRequest(req, recipientToken)
		rec := httptest.NewRecorder()
		env.Router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 for the share recipient", path, rec.Code)
			continue
		}
		raw := rec.Body.String()
		for _, leak := range []string{owner.ID, owner.DisplayName, owner.Email} {
			if leak != "" && strings.Contains(raw, leak) {
				t.Errorf("%s must not carry clicker identity %q for a caller without CanManageShares; body=%s", path, leak, raw)
			}
		}
	}
}
