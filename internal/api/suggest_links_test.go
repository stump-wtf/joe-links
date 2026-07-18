// API tests for GET /api/v1/links/suggest, named after the SPEC-0019 REQ
// "Suggest Endpoint" scenarios so the spec↔test mapping is auditable.
//
// Governing: SPEC-0019 REQ "Suggest Endpoint", ADR-0019
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/api"
	"github.com/joestump/joe-links/internal/store"
)

// getSuggest performs GET /links/suggest with the given raw query string
// (e.g. "?q=ji") and optional bearer token.
func getSuggest(t *testing.T, env *testEnv, token, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/links/suggest"+rawQuery, nil)
	if token != "" {
		authRequest(req, token)
	}
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)
	return rec
}

// decodeSuggestions decodes a 200 response body into the suggestions payload.
func decodeSuggestions(t *testing.T, rec *httptest.ResponseRecorder) api.SuggestLinksResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp api.SuggestLinksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
	return resp
}

// suggestionSlugs extracts slugs from a suggestions payload, in order.
func suggestionSlugs(resp api.SuggestLinksResponse) []string {
	out := make([]string, len(resp.Suggestions))
	for i, s := range resp.Suggestions {
		out[i] = s.Slug
	}
	return out
}

// mustCreateLink creates a link owned by ownerID, failing the test on error.
func mustCreateLink(t *testing.T, env *testEnv, slug, ownerID, title, visibility string) *store.Link {
	t.Helper()
	l, err := env.LinkStore.Create(context.Background(), slug, "https://example.com", ownerID, title, "", visibility)
	if err != nil {
		t.Fatalf("create %q: %v", slug, err)
	}
	return l
}

// Scenario: Prefix Match Ranks First
// WHEN an authenticated user calls GET /api/v1/links/suggest?q=ji and visible
// links jira (slug-prefix), fiji-trip (slug-substring), and docs titled
// "Jira runbook" exist THEN the response lists jira before fiji-trip before
// docs, and contains at most 5 entries.
func TestSuggestLinksAPI_PrefixMatchRanksFirst(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	mustCreateLink(t, env, "docs", user.ID, "Jira runbook", "public")
	mustCreateLink(t, env, "fiji-trip", user.ID, "", "public")
	mustCreateLink(t, env, "jira", user.ID, "", "public")

	resp := decodeSuggestions(t, getSuggest(t, env, token, "?q=ji"))
	got := suggestionSlugs(resp)
	want := []string{"jira", "fiji-trip", "docs"}
	if len(got) != len(want) {
		t.Fatalf("suggestions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("suggestions = %v, want %v", got, want)
		}
	}
	if len(resp.Suggestions) > 5 {
		t.Errorf("got %d suggestions, want at most 5", len(resp.Suggestions))
	}
	// Entries carry the (possibly empty) title.
	if resp.Suggestions[2].Title != "Jira runbook" {
		t.Errorf("docs title = %q, want %q", resp.Suggestions[2].Title, "Jira runbook")
	}
}

// Scenario: Other Users' Private Links Excluded
// WHEN user A calls the suggest endpoint with a query matching user B's
// private link slug THEN the response MUST NOT contain that link.
func TestSuggestLinksAPI_OtherUsersPrivateLinksExcluded(t *testing.T) {
	env := newTestEnv(t)
	alice := seedUser(t, env, "alice@example.com", "user")
	bob := seedUser(t, env, "bob@example.com", "user")
	token := seedToken(t, env, alice.ID)

	mustCreateLink(t, env, "jibberish", bob.ID, "", "private")

	resp := decodeSuggestions(t, getSuggest(t, env, token, "?q=jibberish"))
	if len(resp.Suggestions) != 0 {
		t.Fatalf("suggestions = %v, want none", suggestionSlugs(resp))
	}
}

// Scenario: Shared Secure Link Included
// WHEN a user with a link_shares record for a secure link queries a prefix of
// its slug THEN that link MUST appear in the suggestions.
func TestSuggestLinksAPI_SharedSecureLinkIncluded(t *testing.T) {
	env := newTestEnv(t)
	alice := seedUser(t, env, "alice@example.com", "user")
	bob := seedUser(t, env, "bob@example.com", "user")
	token := seedToken(t, env, alice.ID)

	link := mustCreateLink(t, env, "jitsu", bob.ID, "", "secure")
	if err := env.LinkStore.AddShare(context.Background(), link.ID, alice.ID, bob.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	resp := decodeSuggestions(t, getSuggest(t, env, token, "?q=jit"))
	got := suggestionSlugs(resp)
	if len(got) != 1 || got[0] != "jitsu" {
		t.Fatalf("suggestions = %v, want [jitsu]", got)
	}
}

// Scenario: Unauthenticated Request Rejected
// WHEN GET /api/v1/links/suggest?q=ji is called without an Authorization
// header (with or without a valid session cookie) THEN the server MUST
// respond 401 Unauthorized and reveal no suggestion data.
func TestSuggestLinksAPI_UnauthenticatedRequestRejected(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	mustCreateLink(t, env, "jira", user.ID, "", "public")

	// No Authorization header at all.
	rec := getSuggest(t, env, "", "?q=ji")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}

	// A session cookie is not a bearer token: still 401 (SPEC-0006 REQ
	// "No Web UI Session on API Routes").
	req := httptest.NewRequest("GET", "/links/suggest?q=ji", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "some-session-value"})
	rec = httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status with session cookie = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "suggestions") || strings.Contains(rec.Body.String(), "jira") {
		t.Errorf("401 body leaks suggestion data: %s", rec.Body.String())
	}
}

// Scenario: LIKE Wildcards Neutralized
// WHEN the query is % or _ THEN the characters MUST be matched literally,
// not as SQL wildcards.
func TestSuggestLinksAPI_LikeWildcardsNeutralized(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	mustCreateLink(t, env, "jira", user.ID, "", "public")
	mustCreateLink(t, env, "pct", user.ID, "100% done", "public")

	// A wildcard % would match every link; a literal % matches only the title
	// containing a percent sign.
	resp := decodeSuggestions(t, getSuggest(t, env, token, "?q=%25"))
	got := suggestionSlugs(resp)
	if len(got) != 1 || got[0] != "pct" {
		t.Fatalf("suggestions for %%: %v, want [pct]", got)
	}

	// A wildcard _ would match any single character; nothing here contains a
	// literal underscore.
	resp = decodeSuggestions(t, getSuggest(t, env, token, "?q=_"))
	if len(resp.Suggestions) != 0 {
		t.Fatalf("suggestions for _: %v, want none", suggestionSlugs(resp))
	}
}

// A non-numeric limit is rejected with 400; non-positive limits are rejected
// the same way (SPEC-0019 REQ "Suggest Endpoint").
func TestSuggestLinksAPI_NonNumericLimitRejected(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	for _, limit := range []string{"abc", "5x", "0", "-3"} {
		rec := getSuggest(t, env, token, "?q=ji&limit="+limit)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%q: status = %d, want 400; body: %s", limit, rec.Code, rec.Body.String())
			continue
		}
		var errResp struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Errorf("limit=%q: decode error body: %v", limit, err)
			continue
		}
		if errResp.Code != "BAD_REQUEST" {
			t.Errorf("limit=%q: code = %q, want BAD_REQUEST", limit, errResp.Code)
		}
	}
}

// Result count is capped: default 5, an explicit limit above 10 is clamped
// to 10 (SPEC-0019 REQ "Suggest Endpoint").
func TestSuggestLinksAPI_LimitDefaultAndClamp(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	for i := 0; i < 12; i++ {
		mustCreateLink(t, env, fmt.Sprintf("lim-%02d", i), user.ID, "", "public")
	}

	resp := decodeSuggestions(t, getSuggest(t, env, token, "?q=lim"))
	if len(resp.Suggestions) != 5 {
		t.Errorf("default limit: got %d suggestions, want 5", len(resp.Suggestions))
	}

	resp = decodeSuggestions(t, getSuggest(t, env, token, "?q=lim&limit=99"))
	if len(resp.Suggestions) != 10 {
		t.Errorf("limit=99: got %d suggestions, want 10 (clamped)", len(resp.Suggestions))
	}
}

// An empty q returns an empty suggestions array — a JSON [] rather than null,
// and never the whole corpus (SPEC-0019 REQ "Suggest Endpoint").
func TestSuggestLinksAPI_EmptyQueryReturnsEmptyArray(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)
	mustCreateLink(t, env, "jira", user.ID, "", "public")

	rec := getSuggest(t, env, token, "")
	resp := decodeSuggestions(t, rec)
	if len(resp.Suggestions) != 0 {
		t.Fatalf("suggestions = %v, want none", suggestionSlugs(resp))
	}
	if !strings.Contains(rec.Body.String(), `"suggestions":[]`) {
		t.Errorf("body = %s, want a literal empty array for suggestions", rec.Body.String())
	}
}

// Admin callers receive all links regardless of visibility (SPEC-0010 REQ
// "Admin Visibility Override").
func TestSuggestLinksAPI_AdminVisibilityOverride(t *testing.T) {
	env := newTestEnv(t)
	admin := seedUser(t, env, "admin@example.com", "admin")
	bob := seedUser(t, env, "bob@example.com", "user")
	token := seedToken(t, env, admin.ID)

	mustCreateLink(t, env, "jinx-private", bob.ID, "", "private")

	resp := decodeSuggestions(t, getSuggest(t, env, token, "?q=jinx"))
	got := suggestionSlugs(resp)
	if len(got) != 1 || got[0] != "jinx-private" {
		t.Fatalf("suggestions = %v, want [jinx-private]", got)
	}
}
