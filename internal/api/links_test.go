package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/joestump/joe-links/internal/api"
	"github.com/joestump/joe-links/internal/store"
)

func TestLinks_List_OK(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	// Create a link so the list isn't empty.
	_, err := env.LinkStore.Create(context.Background(), "test-link", "https://example.com", user.ID, "Test", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	req := httptest.NewRequest("GET", "/links", nil)
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
		t.Errorf("len(links) = %d, want 1", len(resp.Links))
	}
}

func TestLinks_List_Unauthenticated(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest("GET", "/links", nil)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestLinks_Create_Created(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	body := `{"slug":"my-new-link","url":"https://example.com","title":"New Link","description":"A new link"}`
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp api.LinkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Slug != "my-new-link" {
		t.Errorf("slug = %q, want %q", resp.Slug, "my-new-link")
	}
	if resp.URL != "https://example.com" {
		t.Errorf("url = %q, want %q", resp.URL, "https://example.com")
	}
	if len(resp.Owners) != 1 {
		t.Errorf("len(owners) = %d, want 1", len(resp.Owners))
	}
}

func TestLinks_Create_DuplicateSlug(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	// Create first link.
	_, err := env.LinkStore.Create(context.Background(), "dup-slug", "https://a.com", user.ID, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	body := `{"slug":"dup-slug","url":"https://b.com"}`
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestLinks_Create_Unauthenticated(t *testing.T) {
	env := newTestEnv(t)
	body := `{"slug":"no-auth","url":"https://example.com"}`
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestLinks_Get_Found(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	link, err := env.LinkStore.Create(context.Background(), "get-me", "https://example.com", user.ID, "Get Me", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest("GET", "/links/"+link.ID, nil)
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp api.LinkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Slug != "get-me" {
		t.Errorf("slug = %q, want %q", resp.Slug, "get-me")
	}
}

func TestLinks_Get_NotFound(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	req := httptest.NewRequest("GET", "/links/nonexistent-id", nil)
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestLinks_Get_Forbidden_NotOwner(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	other := seedUser(t, env, "other@example.com", "user")
	otherToken := seedToken(t, env, other.ID)

	link, err := env.LinkStore.Create(context.Background(), "private-link", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest("GET", "/links/"+link.ID, nil)
	authRequest(req, otherToken)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestLinks_Update_OK(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	link, err := env.LinkStore.Create(context.Background(), "update-me", "https://old.com", user.ID, "Old", "Old desc", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	body := `{"url":"https://new.com","title":"New","description":"New desc"}`
	req := httptest.NewRequest("PUT", "/links/"+link.ID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp api.LinkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.URL != "https://new.com" {
		t.Errorf("url = %q, want %q", resp.URL, "https://new.com")
	}
}

func TestLinks_Update_Forbidden_NotOwner(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	other := seedUser(t, env, "other@example.com", "user")
	otherToken := seedToken(t, env, other.ID)

	link, err := env.LinkStore.Create(context.Background(), "no-update", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	body := `{"url":"https://hacked.com"}`
	req := httptest.NewRequest("PUT", "/links/"+link.ID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, otherToken)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestLinks_Delete_NoContent(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	link, err := env.LinkStore.Create(context.Background(), "delete-me", "https://example.com", user.ID, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest("DELETE", "/links/"+link.ID, nil)
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d; body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
}

func TestLinks_Delete_NotFound(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	req := httptest.NewRequest("DELETE", "/links/nonexistent-id", nil)
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestLinks_Delete_Unauthenticated(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest("DELETE", "/links/some-id", nil)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// Governing: SPEC-0009 REQ "API Representation", ADR-0013
func TestLinks_Create_VariableURL_Passthrough(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	body := `{"slug":"github","url":"https://github.com/$username","title":"GitHub"}`
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp api.LinkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.URL != "https://github.com/$username" {
		t.Errorf("url = %q, want %q — API must return template as-is", resp.URL, "https://github.com/$username")
	}
}

// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
func TestLinks_Create_DuplicateVariable_Rejected(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	body := `{"slug":"dup-var","url":"https://example.com/$foo/$foo"}`
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// Governing: SPEC-0009 REQ "API Representation", ADR-0013
func TestLinks_Get_VariableURL_Passthrough(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	link, err := env.LinkStore.Create(context.Background(), "var-link", "https://example.com/$query/$page", user.ID, "Var Link", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest("GET", "/links/"+link.ID, nil)
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp api.LinkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.URL != "https://example.com/$query/$page" {
		t.Errorf("url = %q, want %q — API must return template as-is", resp.URL, "https://example.com/$query/$page")
	}
}

// Creating a link with duplicate spellings of the same tag must return 201
// with the tag attached once — not roll back the tag write and 500 after the
// link row was created (issue #198).
func TestLinks_Create_DuplicateTags_Deduped(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	body := `{"slug":"jira-board","url":"https://example.com","tags":["Jira","jira"]}`
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp api.LinkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tags) != 1 || resp.Tags[0] != "Jira" {
		t.Errorf("tags = %v, want exactly [Jira]", resp.Tags)
	}
}

// Updating a link with duplicate tags must return 200 with the tag attached once.
func TestLinks_Update_DuplicateTags_Deduped(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	link, err := env.LinkStore.Create(context.Background(), "edit-me", "https://example.com", user.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	body := `{"url":"https://example.com","tags":["Jira","jira","JIRA"]}`
	req := httptest.NewRequest("PUT", "/links/"+link.ID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp api.LinkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tags) != 1 || resp.Tags[0] != "Jira" {
		t.Errorf("tags = %v, want exactly [Jira]", resp.Tags)
	}
}

// If the tag write fails during create, the link and its tags roll back
// together: the client gets a 500 with no half-created link, so a retry does
// not 409 on its own slug (issue #198).
func TestLinks_Create_TagWriteFailure_NoHalfCreatedLink(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	// Force every link_tags write to fail.
	env.DB.MustExec("DROP TABLE link_tags")

	body := `{"slug":"doomed","url":"https://example.com","tags":["jira"]}`
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	if _, err := env.LinkStore.GetBySlug(context.Background(), "doomed"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug after failed create = %v, want ErrNotFound (no half-created resource)", err)
	}
}

// Reserved-slug convergence (#204): the API uses the same exact-match store
// rule as the form and live checker — every reserved word is rejected with
// INVALID_SLUG, and dash-prefixed slugs like "u-foo" are accepted.
// Governing: SPEC-0005 REQ "Links Collection"
func TestLinks_Create_ReservedSlugs_Rejected(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	for _, slug := range store.ReservedSlugs() {
		body := `{"slug":"` + slug + `","url":"https://example.com"}`
		req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		authRequest(req, token)
		rec := httptest.NewRecorder()
		env.Router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /links slug=%q: status = %d, want %d; body: %s", slug, rec.Code, http.StatusBadRequest, rec.Body.String())
			continue
		}
		var resp struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Errorf("slug=%q decode: %v", slug, err)
			continue
		}
		if resp.Code != "INVALID_SLUG" {
			t.Errorf("slug=%q code = %q, want INVALID_SLUG", slug, resp.Code)
		}
	}
}

func TestLinks_Create_DashPrefixedSlug_Accepted(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	body := `{"slug":"u-foo","url":"https://example.com/u-foo"}`
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if _, err := env.LinkStore.GetBySlug(context.Background(), "u-foo"); err != nil {
		t.Errorf("GetBySlug(u-foo) = %v, want link", err)
	}
}

// postLinkExpectingError posts a create body and asserts a 400 with the given
// error code (issues #251, #265).
func postLinkExpectingError(t *testing.T, env *testEnv, token, body, wantCode string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/links", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req, token)
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != wantCode {
		t.Errorf("code = %q, want %q (error: %s)", resp.Code, wantCode, resp.Error)
	}
}

// javascript:/data: URLs are rejected with 400 INVALID_URL on the REST create
// surface (issue #265).
func TestLinks_Create_NonHTTPScheme_Rejected(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	for _, badURL := range []string{"javascript:alert(1)", "data:text/html,x", "//evil.example.com"} {
		body, _ := json.Marshal(map[string]any{"slug": "evil-url", "url": badURL})
		postLinkExpectingError(t, env, token, string(body), "INVALID_URL")
		if _, err := env.LinkStore.GetBySlug(context.Background(), "evil-url"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("url=%q: link exists after rejected create, err = %v", badURL, err)
		}
	}
}

// Hostile tag names are rejected with 400 INVALID_TAG_NAME before any write —
// defense in depth for the stored XSS class fixed at the output layer in #246
// (issue #251).
func TestLinks_Create_HostileTagName_Rejected(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	body, _ := json.Marshal(map[string]any{
		"slug": "evil-tags", "url": "https://example.com",
		"tags": []string{`x');fetch('/evil')//`},
	})
	postLinkExpectingError(t, env, token, string(body), "INVALID_TAG_NAME")
	if _, err := env.LinkStore.GetBySlug(context.Background(), "evil-tags"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("link exists after rejected create, err = %v", err)
	}
}

// More than MaxTagsPerLink tags in one create is rejected with 400
// TOO_MANY_TAGS (issue #265).
func TestLinks_Create_TooManyTags_Rejected(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	tags := make([]string, store.MaxTagsPerLink+1)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag-%d", i)
	}
	body, _ := json.Marshal(map[string]any{"slug": "too-tagged", "url": "https://example.com", "tags": tags})
	postLinkExpectingError(t, env, token, string(body), "TOO_MANY_TAGS")
}

// Update rejects javascript: URLs and hostile tag names with the same codes as
// create, and leaves the link untouched (issues #251, #265).
func TestLinks_Update_HostileInputs_Rejected(t *testing.T) {
	env := newTestEnv(t)
	user := seedUser(t, env, "alice@example.com", "user")
	token := seedToken(t, env, user.ID)

	link, err := env.LinkStore.Create(context.Background(), "target", "https://example.com", user.ID, "Keep", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	cases := []struct {
		name     string
		body     map[string]any
		wantCode string
	}{
		{"javascript url", map[string]any{"url": "javascript:alert(1)"}, "INVALID_URL"},
		{"hostile tag", map[string]any{"url": "https://example.com/new", "tags": []string{`x');fetch('/evil')//`}}, "INVALID_TAG_NAME"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.body)
			req := httptest.NewRequest("PUT", "/links/"+link.ID, bytes.NewBuffer(b))
			req.Header.Set("Content-Type", "application/json")
			authRequest(req, token)
			rec := httptest.NewRecorder()
			env.Router.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			var resp struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", resp.Code, tc.wantCode)
			}

			// The link row is untouched — validation runs before any write.
			got, err := env.LinkStore.GetByID(context.Background(), link.ID)
			if err != nil {
				t.Fatalf("reload link: %v", err)
			}
			if got.URL != "https://example.com" || got.Title != "Keep" {
				t.Errorf("link modified by rejected update: url=%q title=%q", got.URL, got.Title)
			}
		})
	}
}
