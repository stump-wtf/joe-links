// Story #277 — click breakdowns API (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "Referrers grouped by host"
//   - "Recipient gets breakdowns without attribution"
//   - "Top-10 plus Other"
//
// Governing: SPEC-0021 REQ "Click Breakdowns", REQ "Capability Gating of Analytics Surfaces"
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

// breakdownsBody mirrors BreakdownsResponse for decoding.
type breakdownsBody struct {
	LinkID    string `json:"link_id"`
	Days      int    `json:"days"`
	Referrers []struct {
		Host  string `json:"host"`
		Count int64  `json:"count"`
	} `json:"referrers"`
	Browsers []struct {
		Name  string `json:"name"`
		Count int64  `json:"count"`
	} `json:"browsers"`
	OS []struct {
		Name  string `json:"name"`
		Count int64  `json:"count"`
	} `json:"os"`
	Auth struct {
		Authenticated int64 `json:"authenticated"`
		Anonymous     int64 `json:"anonymous"`
	} `json:"auth"`
}

// getBreakdowns performs an authenticated GET and returns the recorder.
func getBreakdowns(t *testing.T, env *testEnv, linkID, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/links/"+linkID+"/stats/breakdowns"+query, nil)
	if token != "" {
		authRequest(req, token)
	}
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)
	return rec
}

// seedBreakdownClick inserts a click row with explicit referrer, user agent,
// and user (empty = anonymous) at the given clicked_at.
func seedBreakdownClick(t *testing.T, env *testEnv, linkID, id, referrer, ua, userID string, ts time.Time) {
	t.Helper()
	var uid interface{}
	if userID != "" {
		uid = userID
	}
	_, err := env.DB.ExecContext(context.Background(),
		env.DB.Rebind(`INSERT INTO link_clicks (id, link_id, user_id, ip_hash, user_agent, referrer, clicked_at) VALUES (?, ?, ?, 'h', ?, ?, ?)`),
		id, linkID, uid, ua, referrer, ts)
	if err != nil {
		t.Fatalf("insert click %s: %v", id, err)
	}
}

// Scenario: Referrers grouped by host — clicks carrying referrers
// https://a.example/x and https://a.example/y?z=1 plus one empty referrer
// yield a.example: 2 and Direct / unknown: 1.
// Governing: SPEC-0021 REQ "Click Breakdowns"
func TestBreakdowns_ReferrersGroupedByHost(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "bd-hosts@example.com", "user")
	token := seedToken(t, env, owner.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "bd-hosts", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	now := time.Now().UTC()
	seedBreakdownClick(t, env, link.ID, "bd-h-1", "https://a.example/x", "", "", now)
	seedBreakdownClick(t, env, link.ID, "bd-h-2", "https://a.example/y?z=1", "", "", now)
	seedBreakdownClick(t, env, link.ID, "bd-h-3", "", "", owner.ID, now)

	rec := getBreakdowns(t, env, link.ID, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp breakdownsBody
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LinkID != link.ID || resp.Days != 30 {
		t.Errorf("link_id/days = %q/%d, want %q/30", resp.LinkID, resp.Days, link.ID)
	}

	byHost := map[string]int64{}
	for _, r := range resp.Referrers {
		byHost[r.Host] = r.Count
	}
	if byHost["a.example"] != 2 {
		t.Errorf("a.example count = %d, want 2 (grouped across path and query variants)", byHost["a.example"])
	}
	if byHost["Direct / unknown"] != 1 {
		t.Errorf("Direct / unknown count = %d, want 1", byHost["Direct / unknown"])
	}

	// The auth split covers the same rows: 1 authenticated, 2 anonymous.
	if resp.Auth.Authenticated != 1 || resp.Auth.Anonymous != 2 {
		t.Errorf("auth split = %d/%d, want 1 authenticated / 2 anonymous", resp.Auth.Authenticated, resp.Auth.Anonymous)
	}
}

// Scenario: Recipient gets breakdowns without attribution — a share recipient
// calling the breakdowns endpoint receives all three breakdowns, and no field
// anywhere in them names or identifies any user.
// Governing: SPEC-0021 REQ "Click Breakdowns", REQ "Capability Gating of Analytics Surfaces"
func TestBreakdowns_RecipientGetsBreakdownsWithoutAttribution(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "bd-share-owner@example.com", "user")
	recipient := seedUser(t, env, "bd-share-recipient@example.com", "user")
	recipientToken := seedToken(t, env, recipient.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "bd-shared", "https://example.com", owner.ID, "Shared", "", "secure")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := env.LinkStore.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	// An authenticated click by the owner: attribution exists in the rows but
	// must not surface in any breakdown field.
	now := time.Now().UTC()
	seedBreakdownClick(t, env, link.ID, "bd-s-1", "https://news.ycombinator.com/item?id=1",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0", owner.ID, now)
	seedBreakdownClick(t, env, link.ID, "bd-s-2", "", "curl/8.6.0", owner.ID, now)

	rec := getBreakdowns(t, env, link.ID, recipientToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("recipient status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	var resp breakdownsBody
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Referrers) == 0 || len(resp.Browsers) == 0 || len(resp.OS) == 0 {
		t.Errorf("recipient must receive all three breakdowns in full; body: %s", body)
	}
	if resp.Auth.Authenticated != 2 || resp.Auth.Anonymous != 0 {
		t.Errorf("auth split = %d/%d, want 2/0", resp.Auth.Authenticated, resp.Auth.Anonymous)
	}

	// No clicker identity anywhere in the raw response (PR #255 rule).
	if strings.Contains(body, owner.ID) {
		t.Errorf("response leaks the clicker's user ID; body: %s", body)
	}
	if strings.Contains(body, owner.DisplayName) {
		t.Errorf("response leaks the clicker's display name; body: %s", body)
	}
	if strings.Contains(body, "user_id") || strings.Contains(body, "display_name") {
		t.Errorf("response carries a user-identity field; body: %s", body)
	}
}

// Scenario: Top-10 plus Other — clicks from 14 distinct referrer hosts list
// the 10 largest hosts and sum the remaining 4 into a single "Other" row.
// Governing: SPEC-0021 REQ "Click Breakdowns"
func TestBreakdowns_Top10PlusOther(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "bd-top10@example.com", "user")
	token := seedToken(t, env, owner.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "bd-top10", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	// 14 distinct hosts: host-1 gets 15 clicks, host-2 gets 14, … host-14
	// gets 2 — so hosts 1–10 are the top 10 and hosts 11–14 (5+4+3+2 = 14
	// clicks) fold into Other.
	now := time.Now().UTC()
	n := 0
	for h := 1; h <= 14; h++ {
		for c := 0; c < 16-h; c++ {
			n++
			seedBreakdownClick(t, env, link.ID, fmt.Sprintf("bd-t-%03d", n),
				fmt.Sprintf("https://host-%d.example/page", h), "", "", now)
		}
	}

	rec := getBreakdowns(t, env, link.ID, token, "?days=30")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp breakdownsBody
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Referrers) != 11 {
		t.Fatalf("referrer rows = %d, want 11 (top 10 + Other); body: %+v", len(resp.Referrers), resp.Referrers)
	}
	// Top 10 hosts descending by count.
	for i := 0; i < 10; i++ {
		wantHost := fmt.Sprintf("host-%d.example", i+1)
		wantCount := int64(15 - i)
		if resp.Referrers[i].Host != wantHost || resp.Referrers[i].Count != wantCount {
			t.Errorf("referrers[%d] = %s:%d, want %s:%d", i, resp.Referrers[i].Host, resp.Referrers[i].Count, wantHost, wantCount)
		}
	}
	// Remaining 4 hosts summed into a single Other row.
	last := resp.Referrers[10]
	if last.Host != "Other" || last.Count != 14 {
		t.Errorf("referrers[10] = %s:%d, want Other:14", last.Host, last.Count)
	}
}

// The days parameter shares the timeseries contract: values other than 30 and
// 90 are 400 in the standard error shape (SPEC-0005).
// Governing: SPEC-0021 REQ "Click Breakdowns"
func TestBreakdowns_InvalidWindowRejected(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "bd-badwindow@example.com", "user")
	token := seedToken(t, env, owner.ID)

	link, err := env.LinkStore.Create(context.Background(), "bd-badwindow", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	for _, days := range []string{"7", "0", "-30", "365", "abc", "030", "+90"} {
		rec := getBreakdowns(t, env, link.ID, token, "?days="+days)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("days=%s: status = %d, want 400; body: %s", days, rec.Code, rec.Body.String())
		}
	}
}

// One matrix, three surfaces agree: an unrelated authenticated user is not a
// CanStats caller and gets 403; unauthenticated is 401; unknown links 404.
// Governing: SPEC-0021 REQ "Click Breakdowns", REQ "Capability Gating of Analytics Surfaces"
func TestBreakdowns_StrangerForbidden(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "bd-authz-owner@example.com", "user")
	stranger := seedUser(t, env, "bd-authz-stranger@example.com", "user")
	strangerToken := seedToken(t, env, stranger.ID)

	link, err := env.LinkStore.Create(context.Background(), "bd-authz", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	if rec := getBreakdowns(t, env, link.ID, strangerToken, ""); rec.Code != http.StatusForbidden {
		t.Errorf("stranger status = %d, want 403", rec.Code)
	}
	if rec := getBreakdowns(t, env, link.ID, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
	if rec := getBreakdowns(t, env, "nonexistent-id", strangerToken, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown link status = %d, want 404", rec.Code)
	}
}
