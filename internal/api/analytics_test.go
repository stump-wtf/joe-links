// Story #278 — global analytics API (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "Shared link included for recipient"
//   - "Admin toggle"
//   - "Trend against previous period"
//
// ("No cross-user leakage" is pinned against the web dashboard in
// internal/handler/analytics_page_test.go, the surface the scenario names;
// the personal-scope assertions of the admin-toggle test cover the API side.)
//
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces", ADR-0021
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// analyticsBody mirrors AnalyticsResponse for decoding.
type analyticsBody struct {
	Period   string `json:"period"`
	Scope    string `json:"scope"`
	TopLinks []struct {
		LinkID        string   `json:"link_id"`
		Slug          string   `json:"slug"`
		Count         int64    `json:"count"`
		PreviousCount int64    `json:"previous_count"`
		TrendPct      *float64 `json:"trend_pct"`
	} `json:"top_links"`
	NeverClicked []struct {
		LinkID    string    `json:"link_id"`
		Slug      string    `json:"slug"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"never_clicked"`
	TopReferrers []struct {
		Host  string `json:"host"`
		Count int64  `json:"count"`
	} `json:"top_referrers"`
}

// getAnalytics performs an authenticated GET /analytics and returns the recorder.
func getAnalytics(t *testing.T, env *testEnv, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/analytics"+query, nil)
	if token != "" {
		authRequest(req, token)
	}
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)
	return rec
}

// decodeAnalytics fails the test unless rec is a 200 with a valid body.
func decodeAnalytics(t *testing.T, rec *httptest.ResponseRecorder) analyticsBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body analyticsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode analytics response: %v; body=%s", err, rec.Body.String())
	}
	return body
}

// seedAnalyticsClicks inserts n clicks on a link at ts, with distinct ids.
func seedAnalyticsClicks(t *testing.T, env *testEnv, linkID string, n int, ts time.Time, referrer string) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedClickAt(t, env, linkID, fmt.Sprintf("an-%s-%d-%d", linkID[:8], ts.Unix(), i), referrer, ts)
	}
}

// analyticsThisWeek and analyticsLastWeek return instants inside the current
// week window (today's UTC day plus the preceding 6 whole days) and the
// previous equal-length window respectively.
func analyticsThisWeek(t *testing.T) time.Time {
	t.Helper()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return today.AddDate(0, 0, -1).Add(12 * time.Hour)
}

func analyticsLastWeek(t *testing.T) time.Time {
	t.Helper()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return today.AddDate(0, 0, -8).Add(12 * time.Hour)
}

// Scenario: Shared link included for recipient — a secure link shared with
// user A that receives clicks this week is eligible for A's top-links panel
// (counts only, no clicker identities).
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
func TestAnalytics_SharedLinkIncludedForRecipient(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "an-share-owner@example.com", "user")
	recipient := seedUser(t, env, "an-share-recipient@example.com", "user")
	token := seedToken(t, env, recipient.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "an-shared", "https://example.com/shared", owner.ID, "Shared", "", "secure")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := env.LinkStore.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}
	// Authenticated clicks by the owner: attribution exists in the rows but
	// must never surface on this surface.
	ts := analyticsThisWeek(t)
	for i := 0; i < 3; i++ {
		seedBreakdownClick(t, env, link.ID, fmt.Sprintf("an-shared-%d", i), "https://ref.example/x", "", owner.ID, ts)
	}

	body := decodeAnalytics(t, getAnalytics(t, env, token, ""))
	found := false
	for _, tl := range body.TopLinks {
		if tl.LinkID == link.ID {
			found = true
			if tl.Slug != "an-shared" || tl.Count != 3 {
				t.Errorf("shared link row = %+v, want slug an-shared count 3", tl)
			}
		}
	}
	if !found {
		t.Fatalf("shared link must be eligible for the recipient's top-links panel; top_links=%+v", body.TopLinks)
	}

	// Counts only: no clicker identity anywhere in the response (PR #255 rule).
	raw := getAnalytics(t, env, token, "").Body.String()
	for _, leak := range []string{owner.ID, owner.DisplayName, `"user"`, "display_name"} {
		if leak != "" && strings.Contains(raw, leak) {
			t.Errorf("analytics response must not carry clicker identity %q; body=%s", leak, raw)
		}
	}
}

// Scenario: Admin toggle — an admin requesting scope=all receives
// instance-wide aggregates, a non-admin requesting scope=all receives 403,
// and without the parameter both receive their personal scope.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", REQ "Capability Gating of Analytics Surfaces"
func TestAnalytics_AdminToggle(t *testing.T) {
	env := newTestEnv(t)
	admin := seedUser(t, env, "an-admin@example.com", "admin")
	member := seedUser(t, env, "an-member@example.com", "user")
	adminToken := seedToken(t, env, admin.ID)
	memberToken := seedToken(t, env, member.ID)
	ctx := context.Background()

	// The member owns a clicked link; the admin owns nothing.
	link, err := env.LinkStore.Create(ctx, "an-member-link", "https://example.com/member", member.ID, "Member", "", "public")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	seedAnalyticsClicks(t, env, link.ID, 4, analyticsThisWeek(t), "https://news.example/hn")

	// Admin with scope=all: instance-wide aggregates include the member's link.
	all := decodeAnalytics(t, getAnalytics(t, env, adminToken, "?scope=all"))
	if all.Scope != "all" {
		t.Errorf("scope = %q, want all", all.Scope)
	}
	if len(all.TopLinks) != 1 || all.TopLinks[0].LinkID != link.ID {
		t.Errorf("admin scope=all top_links = %+v, want the member's link", all.TopLinks)
	}

	// Non-admin with scope=all: 403 in the standard error shape.
	rec := getAnalytics(t, env, memberToken, "?scope=all")
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin scope=all status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// Without the parameter both receive their personal scope: the admin —
	// whose CanStats is universal — still sees only own/co-owned/shared
	// links, which for this admin is nothing.
	adminMine := decodeAnalytics(t, getAnalytics(t, env, adminToken, ""))
	if adminMine.Scope != "mine" {
		t.Errorf("default scope = %q, want mine", adminMine.Scope)
	}
	if len(adminMine.TopLinks) != 0 || len(adminMine.TopReferrers) != 0 || len(adminMine.NeverClicked) != 0 {
		t.Errorf("admin default scope must be personal, not instance-wide; got %+v", adminMine)
	}
	memberMine := decodeAnalytics(t, getAnalytics(t, env, memberToken, ""))
	if len(memberMine.TopLinks) != 1 || memberMine.TopLinks[0].LinkID != link.ID {
		t.Errorf("member personal scope top_links = %+v, want the member's own link", memberMine.TopLinks)
	}
}

// Scenario: Trend against previous period — a link with 120 clicks this week
// and 80 the previous week shows +50%, and a link with 5 this week and 0 the
// previous week carries the "new" marker (trend_pct: null), never Infinity or
// a division error.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard"
func TestAnalytics_TrendAgainstPreviousPeriod(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "an-trend@example.com", "user")
	token := seedToken(t, env, owner.ID)
	ctx := context.Background()

	grown, err := env.LinkStore.Create(ctx, "an-grown", "https://example.com/grown", owner.ID, "Grown", "", "private")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	fresh, err := env.LinkStore.Create(ctx, "an-fresh", "https://example.com/fresh", owner.ID, "Fresh", "", "private")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	seedAnalyticsClicks(t, env, grown.ID, 120, analyticsThisWeek(t), "")
	seedAnalyticsClicks(t, env, grown.ID, 80, analyticsLastWeek(t), "")
	seedAnalyticsClicks(t, env, fresh.ID, 5, analyticsThisWeek(t), "")

	body := decodeAnalytics(t, getAnalytics(t, env, token, "?period=week"))
	if len(body.TopLinks) != 2 {
		t.Fatalf("top_links length = %d, want 2; %+v", len(body.TopLinks), body.TopLinks)
	}

	// Ranked by current-period count: grown (120) before fresh (5).
	g, f := body.TopLinks[0], body.TopLinks[1]
	if g.Slug != "an-grown" || f.Slug != "an-fresh" {
		t.Fatalf("top_links order = %q, %q; want an-grown, an-fresh", g.Slug, f.Slug)
	}
	if g.Count != 120 || g.PreviousCount != 80 {
		t.Errorf("grown counts = %d/%d, want 120/80", g.Count, g.PreviousCount)
	}
	if g.TrendPct == nil || *g.TrendPct != 50.0 {
		t.Errorf("grown trend_pct = %v, want 50.0", g.TrendPct)
	}
	if f.Count != 5 || f.PreviousCount != 0 {
		t.Errorf("fresh counts = %d/%d, want 5/0", f.Count, f.PreviousCount)
	}
	if f.TrendPct != nil {
		t.Errorf("fresh trend_pct = %v, want null (the new marker case)", *f.TrendPct)
	}
}

// The optional period and scope parameters accept only their documented
// values; anything else is a 400 in the standard error shape, and the route
// requires a bearer token like every /api/v1 route.
// Governing: SPEC-0021 REQ "Global Analytics Dashboard", SPEC-0005, SPEC-0006
func TestAnalytics_ParameterValidationAndAuth(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "an-params@example.com", "user")
	token := seedToken(t, env, user.ID)

	if rec := getAnalytics(t, env, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
	if rec := getAnalytics(t, env, token, "?period=fortnight"); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid period status = %d, want 400", rec.Code)
	}
	if rec := getAnalytics(t, env, token, "?scope=everyone"); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid scope status = %d, want 400", rec.Code)
	}

	body := decodeAnalytics(t, getAnalytics(t, env, token, "?period=month"))
	if body.Period != "month" {
		t.Errorf("period = %q, want month", body.Period)
	}
}
