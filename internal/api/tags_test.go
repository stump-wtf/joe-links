package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/api"
	"github.com/joestump/joe-links/internal/store"
)

// seedTaggedLink creates a link with the given owner, visibility, and tags.
func seedTaggedLink(t *testing.T, env *testEnv, ownerID, slug, visibility string, tagNames ...string) *store.Link {
	t.Helper()
	ctx := context.Background()
	l, err := env.LinkStore.Create(ctx, slug, "https://internal.example.com/"+slug, ownerID, "", "", visibility)
	if err != nil {
		t.Fatalf("create link %q: %v", slug, err)
	}
	if err := env.LinkStore.SetTags(ctx, l.ID, tagNames); err != nil {
		t.Fatalf("tag link %q: %v", slug, err)
	}
	return l
}

// getTags performs an authenticated GET and decodes the response into out.
func apiGet(t *testing.T, env *testEnv, token, path string, wantStatus int, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("GET %s status = %d, want %d; body: %s", path, rec.Code, wantStatus, rec.Body.String())
	}
	if out != nil {
		if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return rec
}

// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering", REQ "Admin
// Visibility Override" — GET /api/v1/tags must not enumerate tag names or
// counts derived from links invisible to the caller (issue #244).
func TestTags_List_VisibilityFiltering(t *testing.T) {
	env := newTestEnv(t)
	alice := seedUser(t, env, "alice@example.com", "user")
	aliceToken := seedToken(t, env, alice.ID)
	bob := seedUser(t, env, "bob@example.com", "user")
	bobToken := seedToken(t, env, bob.ID)
	admin := seedUser(t, env, "admin@example.com", "admin")
	adminToken := seedToken(t, env, admin.ID)

	seedTaggedLink(t, env, alice.ID, "handbook", "public", "handbook")
	seedTaggedLink(t, env, alice.ID, "payroll", "secure", "handbook", "layoffs-2026")

	counts := func(resp *api.TagListResponse) map[string]int {
		m := make(map[string]int, len(resp.Tags))
		for _, tag := range resp.Tags {
			m[tag.Slug] = tag.LinkCount
		}
		return m
	}

	// Bob must not see the secure-only tag, and handbook's count must exclude
	// the secure link.
	var resp api.TagListResponse
	apiGet(t, env, bobToken, "/tags", http.StatusOK, &resp)
	if m := counts(&resp); len(m) != 1 || m["handbook"] != 1 {
		t.Errorf("bob tags = %v, want map[handbook:1]", m)
	}

	// The owner sees both tags with full counts.
	resp = api.TagListResponse{}
	apiGet(t, env, aliceToken, "/tags", http.StatusOK, &resp)
	if m := counts(&resp); len(m) != 2 || m["handbook"] != 2 || m["layoffs-2026"] != 1 {
		t.Errorf("alice tags = %v, want map[handbook:2 layoffs-2026:1]", m)
	}

	// Admins keep the unfiltered list.
	resp = api.TagListResponse{}
	apiGet(t, env, adminToken, "/tags", http.StatusOK, &resp)
	if m := counts(&resp); len(m) != 2 || m["handbook"] != 2 || m["layoffs-2026"] != 1 {
		t.Errorf("admin tags = %v, want map[handbook:2 layoffs-2026:1]", m)
	}
}

// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — GET
// /api/v1/tags/{slug}/links returns public, owned, and shared links for
// non-admins (issue #244).
func TestTags_ListLinks_VisibilityFiltering(t *testing.T) {
	env := newTestEnv(t)
	alice := seedUser(t, env, "alice@example.com", "user")
	bob := seedUser(t, env, "bob@example.com", "user")
	bobToken := seedToken(t, env, bob.ID)
	admin := seedUser(t, env, "admin@example.com", "admin")
	adminToken := seedToken(t, env, admin.ID)

	seedTaggedLink(t, env, alice.ID, "handbook", "public", "work")
	seedTaggedLink(t, env, alice.ID, "payroll", "secure", "work")

	slugs := func(resp *api.LinkListResponse) map[string]bool {
		m := make(map[string]bool, len(resp.Links))
		for _, l := range resp.Links {
			m[l.Slug] = true
		}
		return m
	}

	// Bob sees alice's public link (not just owned links) but not her secure one.
	var resp api.LinkListResponse
	apiGet(t, env, bobToken, "/tags/work/links", http.StatusOK, &resp)
	if m := slugs(&resp); len(m) != 1 || !m["handbook"] {
		t.Errorf("bob links = %v, want map[handbook:true]", m)
	}

	// Admins see everything.
	resp = api.LinkListResponse{}
	apiGet(t, env, adminToken, "/tags/work/links", http.StatusOK, &resp)
	if m := slugs(&resp); len(m) != 2 || !m["handbook"] || !m["payroll"] {
		t.Errorf("admin links = %v, want map[handbook:true payroll:true]", m)
	}
}

// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — a tag whose
// links are all invisible to the caller must 404 exactly like a nonexistent
// tag, closing the API tag-existence oracle (issue #244; API analog of the
// dashboard fix in PR #241).
func TestTags_ListLinks_InvisibleTagIs404(t *testing.T) {
	env := newTestEnv(t)
	alice := seedUser(t, env, "alice@example.com", "user")
	aliceToken := seedToken(t, env, alice.ID)
	bob := seedUser(t, env, "bob@example.com", "user")
	bobToken := seedToken(t, env, bob.ID)
	admin := seedUser(t, env, "admin@example.com", "admin")
	adminToken := seedToken(t, env, admin.ID)

	link := seedTaggedLink(t, env, alice.ID, "payroll", "secure", "Layoffs 2026")

	// For bob, an invisible tag and a nonexistent tag must be indistinguishable.
	invisible := apiGet(t, env, bobToken, "/tags/layoffs-2026/links", http.StatusNotFound, nil)
	missing := apiGet(t, env, bobToken, "/tags/no-such-tag/links", http.StatusNotFound, nil)
	if invisible.Body.String() != missing.Body.String() {
		t.Errorf("invisible-tag 404 body %q differs from nonexistent-tag 404 body %q (oracle)",
			invisible.Body.String(), missing.Body.String())
	}
	if strings.Contains(invisible.Body.String(), "Layoffs") {
		t.Errorf("404 body leaked the tag name: %s", invisible.Body.String())
	}

	// Owner and admin still get the links.
	var resp api.LinkListResponse
	apiGet(t, env, aliceToken, "/tags/layoffs-2026/links", http.StatusOK, &resp)
	if len(resp.Links) != 1 {
		t.Errorf("alice links = %d, want 1", len(resp.Links))
	}
	resp = api.LinkListResponse{}
	apiGet(t, env, adminToken, "/tags/layoffs-2026/links", http.StatusOK, &resp)
	if len(resp.Links) != 1 {
		t.Errorf("admin links = %d, want 1", len(resp.Links))
	}

	// Sharing the link makes the tag visible to bob.
	if err := env.LinkStore.AddShare(context.Background(), link.ID, bob.ID, alice.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}
	resp = api.LinkListResponse{}
	apiGet(t, env, bobToken, "/tags/layoffs-2026/links", http.StatusOK, &resp)
	if len(resp.Links) != 1 || resp.Links[0].Slug != "payroll" {
		t.Errorf("bob shared links = %+v, want [payroll]", resp.Links)
	}
}

// Governing: SPEC-0005 REQ "API Response Structures" — GET /tags/{slug}/links must
// return the same link shape as other endpoints, including owners, tags, and visibility.
func TestTags_ListLinks_FullResponseShape(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)
	ctx := context.Background()

	link, err := env.LinkStore.Create(ctx, "docs", "https://example.com", user.ID, "Docs", "team docs", "private")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := env.LinkStore.SetTags(ctx, link.ID, []string{"work"}); err != nil {
		t.Fatalf("set tags: %v", err)
	}

	req := httptest.NewRequest("GET", "/tags/work/links", nil)
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp api.LinkListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Links) != 1 {
		t.Fatalf("len(links) = %d, want 1", len(resp.Links))
	}

	got := resp.Links[0]
	if got.Visibility != "private" {
		t.Errorf("visibility = %q, want %q", got.Visibility, "private")
	}
	if len(got.Tags) != 1 || got.Tags[0] != "work" {
		t.Errorf("tags = %v, want [work]", got.Tags)
	}
	if len(got.Owners) != 1 {
		t.Fatalf("len(owners) = %d, want 1", len(got.Owners))
	}
	if got.Owners[0].Email != "alice@example.com" || !got.Owners[0].IsPrimary {
		t.Errorf("owner = %+v, want primary alice@example.com", got.Owners[0])
	}
}
