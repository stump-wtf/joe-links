// Governing: SPEC-0021 REQ "Time Series API", REQ "Capability Gating of Analytics Surfaces"
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/store"
)

// -- GET /api/v1/links/{id}/stats/timeseries --

// timeseriesBody mirrors TimeSeriesResponse for decoding.
type timeseriesBody struct {
	LinkID string `json:"link_id"`
	Days   int    `json:"days"`
	Series []struct {
		Date  string `json:"date"`
		Count int64  `json:"count"`
	} `json:"series"`
}

// getTimeSeries performs an authenticated GET and returns the recorder.
func getTimeSeries(t *testing.T, env *testEnv, linkID, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/links/"+linkID+"/stats/timeseries"+query, nil)
	if token != "" {
		authRequest(req, token)
	}
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)
	return rec
}

// Scenario: Owner fetches series — GET ?days=90 with a valid PAT returns 200
// with exactly 90 ascending gap-filled entries.
// Governing: SPEC-0021 REQ "Time Series API"
func TestTimeSeries_OwnerFetchesSeries(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "ts-owner@example.com", "user")
	token := seedToken(t, env, owner.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "ts-owner-link", "https://example.com", owner.ID, "TS", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	// One click now, two clicks five days ago (well inside the 90-day window).
	if err := env.ClickStore.RecordClick(ctx, store.ClickEvent{
		LinkID: link.ID, UserID: owner.ID, IPHash: "h1", UserAgent: "Test/1",
	}); err != nil {
		t.Fatalf("record click: %v", err)
	}
	fiveDaysAgo := time.Now().UTC().AddDate(0, 0, -5)
	seedClickAt(t, env, link.ID, "ts-click-a", "https://ref.example/a", fiveDaysAgo)
	seedClickAt(t, env, link.ID, "ts-click-b", "https://ref.example/b", fiveDaysAgo)

	rec := getTimeSeries(t, env, link.ID, token, "?days=90")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp timeseriesBody
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LinkID != link.ID {
		t.Errorf("link_id = %q, want %q", resp.LinkID, link.ID)
	}
	if resp.Days != 90 {
		t.Errorf("days = %d, want 90", resp.Days)
	}
	if len(resp.Series) != 90 {
		t.Fatalf("len(series) = %d, want exactly 90", len(resp.Series))
	}

	// Ascending, consecutive UTC calendar days — gap-filled, never sparse.
	var total int64
	for i, e := range resp.Series {
		d, err := time.Parse("2006-01-02", e.Date)
		if err != nil {
			t.Fatalf("series[%d].date %q is not YYYY-MM-DD: %v", i, e.Date, err)
		}
		if i > 0 {
			prev, _ := time.Parse("2006-01-02", resp.Series[i-1].Date)
			if !d.Equal(prev.AddDate(0, 0, 1)) {
				t.Fatalf("series[%d].date %q does not follow %q consecutively", i, e.Date, resp.Series[i-1].Date)
			}
		}
		total += e.Count
	}
	if total != 3 {
		t.Errorf("total clicks across series = %d, want 3", total)
	}
	byDate := map[string]int64{}
	for _, e := range resp.Series {
		byDate[e.Date] = e.Count
	}
	if got := byDate[fiveDaysAgo.Format("2006-01-02")]; got != 2 {
		t.Errorf("count on %s = %d, want 2", fiveDaysAgo.Format("2006-01-02"), got)
	}
}

// The optional days parameter defaults to 30 when absent.
// Governing: SPEC-0021 REQ "Time Series API"
func TestTimeSeries_DefaultWindow30(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "ts-default@example.com", "user")
	token := seedToken(t, env, owner.ID)

	link, err := env.LinkStore.Create(context.Background(), "ts-default", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	rec := getTimeSeries(t, env, link.ID, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp timeseriesBody
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Days != 30 {
		t.Errorf("days = %d, want default 30", resp.Days)
	}
	if len(resp.Series) != 30 {
		t.Errorf("len(series) = %d, want exactly 30", len(resp.Series))
	}
}

// Scenario: Invalid window rejected — days values other than 30 and 90
// return 400 with the standard error shape (SPEC-0005).
// Governing: SPEC-0021 REQ "Time Series API"
func TestTimeSeries_InvalidWindowRejected(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "ts-badwindow@example.com", "user")
	token := seedToken(t, env, owner.ID)

	link, err := env.LinkStore.Create(context.Background(), "ts-badwindow", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	for _, days := range []string{"7", "0", "-30", "365", "abc", "30.0", "9000000000000000000000", "030", "+30", "%2030", "090", "+90"} {
		rec := getTimeSeries(t, env, link.ID, token, "?days="+days)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("days=%s: status = %d, want 400; body: %s", days, rec.Code, rec.Body.String())
			continue
		}
		var body struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Errorf("days=%s: decode error body: %v", days, err)
			continue
		}
		if body.Error == "" || body.Code != "BAD_REQUEST" {
			t.Errorf("days=%s: error body = %+v, want standard shape with code BAD_REQUEST", days, body)
		}
	}
}

// Scenario: Recipient authorized, stranger forbidden — a share recipient gets
// 200 for the shared secure link while an unrelated authenticated user gets
// 403 in the standard error shape.
// Governing: SPEC-0021 REQ "Time Series API", REQ "Capability Gating of Analytics Surfaces"
func TestTimeSeries_RecipientAuthorizedStrangerForbidden(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "ts-share-owner@example.com", "user")
	recipient := seedUser(t, env, "ts-share-recipient@example.com", "user")
	stranger := seedUser(t, env, "ts-share-stranger@example.com", "user")
	recipientToken := seedToken(t, env, recipient.ID)
	strangerToken := seedToken(t, env, stranger.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "ts-shared", "https://example.com", owner.ID, "Shared", "", "secure")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := env.LinkStore.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}
	if err := env.ClickStore.RecordClick(ctx, store.ClickEvent{
		LinkID: link.ID, UserID: owner.ID, IPHash: "h1", UserAgent: "Test/1",
	}); err != nil {
		t.Fatalf("record click: %v", err)
	}

	// Recipient: 200 with the full counts-only series.
	rec := getTimeSeries(t, env, link.ID, recipientToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("recipient status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp timeseriesBody
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode recipient body: %v", err)
	}
	if len(resp.Series) != 30 {
		t.Errorf("recipient len(series) = %d, want 30", len(resp.Series))
	}

	// Stranger: 403 in the standard error shape.
	rec = getTimeSeries(t, env, link.ID, strangerToken, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stranger status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode stranger error body: %v", err)
	}
	if errBody.Code != "FORBIDDEN" {
		t.Errorf("stranger error code = %q, want FORBIDDEN", errBody.Code)
	}
}

// Authorization matrix: owner, co-owner, admin, and share recipient are all
// CanStats callers (200); an unrelated user is not (403).
// Governing: SPEC-0021 REQ "Time Series API", REQ "Capability Gating of Analytics Surfaces"
func TestTimeSeries_AuthzMatrix(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "ts-mx-owner@example.com", "user")
	coOwner := seedUser(t, env, "ts-mx-coowner@example.com", "user")
	admin := seedUser(t, env, "ts-mx-admin@example.com", "admin")
	recipient := seedUser(t, env, "ts-mx-recipient@example.com", "user")
	stranger := seedUser(t, env, "ts-mx-stranger@example.com", "user")
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "ts-matrix", "https://example.com", owner.ID, "Matrix", "", "secure")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := env.OwnershipStore.AddOwner(link.ID, coOwner.ID); err != nil {
		t.Fatalf("add co-owner: %v", err)
	}
	if err := env.LinkStore.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	cases := []struct {
		name string
		user *store.User
		want int
	}{
		{"owner", owner, http.StatusOK},
		{"co-owner", coOwner, http.StatusOK},
		{"admin", admin, http.StatusOK},
		{"share recipient", recipient, http.StatusOK},
		{"stranger", stranger, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := seedToken(t, env, tc.user.ID)
			rec := getTimeSeries(t, env, link.ID, token, "?days=30")
			if rec.Code != tc.want {
				t.Errorf("%s: status = %d, want %d; body: %s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Bearer PAT is required — no token means 401 (SPEC-0006).
// Governing: SPEC-0021 REQ "Time Series API"
func TestTimeSeries_Unauthenticated(t *testing.T) {
	env := newTestEnv(t)

	rec := getTimeSeries(t, env, "some-id", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// Unknown links are 404.
// Governing: SPEC-0021 REQ "Time Series API"
func TestTimeSeries_NotFound(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "ts-nf@example.com", "user")
	token := seedToken(t, env, user.ID)

	rec := getTimeSeries(t, env, "nonexistent-id", token, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
