// REST API tests for the SPEC-0020 REQ "Link Expiration" scenarios: optional
// expires_at on POST /links and PUT /links/{id}, tri-state update semantics
// (omitted = unchanged, null = clear), past-value rejection, and capability
// gating (share recipients cannot touch expiry). Tests are named after the
// spec scenarios so the spec↔test mapping is auditable.
//
// Governing: SPEC-0020 REQ "Link Expiration", ADR-0020
package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// linkJSON is the subset of LinkResponse these tests assert on.
type linkJSON struct {
	ID        string     `json:"id"`
	Slug      string     `json:"slug"`
	Title     string     `json:"title"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// doJSON issues an authenticated JSON request and decodes the response body.
func doJSON(t *testing.T, env *testEnv, token, method, path, body string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.Router.ServeHTTP(rec, authRequest(req, token))
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode response: %v\n%s", method, path, err, rec.Body.String())
		}
	}
	return rec
}

// backdateExpiryAPI forces a link's expires_at into the past via raw SQL.
func backdateExpiryAPI(t *testing.T, env *testEnv, linkID string, expiresAt time.Time) {
	t.Helper()
	if _, err := env.DB.Exec(env.DB.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`),
		expiresAt.UTC().Truncate(time.Second), linkID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
}

// Scenario: Link Created with Expiration
// WHEN a user creates a link with expires_at set to a future timestamp
// THEN the link is persisted with that expires_at.
func TestAPILifecycle_LinkCreatedWithExpiration(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	var created linkJSON
	rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"expires","url":"https://example.com","expires_at":"`+future.Format(time.RFC3339)+`"}`, &created)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if created.ExpiresAt == nil || !created.ExpiresAt.Equal(future) {
		t.Errorf("created expires_at = %v, want %v", created.ExpiresAt, future)
	}

	// GET round-trips it, so clients can re-PUT the full resource.
	var got linkJSON
	if rec := doJSON(t, env, token, http.MethodGet, "/links/"+created.ID, "", &got); rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(future) {
		t.Errorf("GET expires_at = %v, want %v", got.ExpiresAt, future)
	}

	// Omitting expires_at on create yields null (never expires).
	var plain linkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"forever","url":"https://example.com/2"}`, &plain); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	if plain.ExpiresAt != nil {
		t.Errorf("expires_at omitted on create = %v, want null", plain.ExpiresAt)
	}
}

// Scenario: Past Expiration Rejected
// WHEN a create or update sets expires_at to a past timestamp that differs
// from the stored value THEN 400 INVALID_EXPIRES_AT and nothing is persisted.
func TestAPILifecycle_PastExpirationRejected(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"born-dead","url":"https://example.com","expires_at":"`+past+`"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "INVALID_EXPIRES_AT") {
		t.Errorf("error code missing INVALID_EXPIRES_AT: %s", rec.Body.String())
	}

	var link linkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"alive","url":"https://example.com"}`, &link); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}
	rec = doJSON(t, env, token, http.MethodPut, "/links/"+link.ID,
		`{"url":"https://example.com","expires_at":"`+past+`"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var got linkJSON
	doJSON(t, env, token, http.MethodGet, "/links/"+link.ID, "", &got)
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after rejected update = %v, want null", got.ExpiresAt)
	}
}

// Scenario: Expired Link Stays Editable
// WHEN an owner edits an expired link round-tripping the stored (past)
// expires_at unchanged THEN the update succeeds and expires_at is untouched.
func TestAPILifecycle_ExpiredLinkStaysEditable(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	var link linkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"expired","url":"https://example.com","title":"Old"}`, &link); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}
	past := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	backdateExpiryAPI(t, env, link.ID, past)

	// A full-resource PUT naturally sends the stored (past) value back.
	var updated linkJSON
	rec := doJSON(t, env, token, http.MethodPut, "/links/"+link.ID,
		`{"url":"https://example.com","title":"New","expires_at":"`+past.Format(time.RFC3339)+`"}`, &updated)
	if rec.Code != http.StatusOK {
		t.Fatalf("round-trip update status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if updated.Title != "New" {
		t.Errorf("title = %q, want %q", updated.Title, "New")
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(past) {
		t.Errorf("expires_at = %v, want unchanged %v", updated.ExpiresAt, past)
	}

	// Omitting expires_at entirely also leaves the stored value unchanged.
	var omitted linkJSON
	rec = doJSON(t, env, token, http.MethodPut, "/links/"+link.ID,
		`{"url":"https://example.com","title":"Newer"}`, &omitted)
	if rec.Code != http.StatusOK {
		t.Fatalf("omitted-field update status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if omitted.ExpiresAt == nil || !omitted.ExpiresAt.Equal(past) {
		t.Errorf("expires_at after omitted field = %v, want unchanged %v", omitted.ExpiresAt, past)
	}
}

// Scenario: Expiration Cleared on Edit
// WHEN an owner PUTs "expires_at": null THEN stored expires_at becomes NULL.
func TestAPILifecycle_ExpirationClearedOnEdit(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	var link linkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"clear-me","url":"https://example.com","expires_at":"`+future.Format(time.RFC3339)+`"}`, &link); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}

	var updated linkJSON
	rec := doJSON(t, env, token, http.MethodPut, "/links/"+link.ID,
		`{"url":"https://example.com","expires_at":null}`, &updated)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear update status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if updated.ExpiresAt != nil {
		t.Errorf("expires_at after explicit null = %v, want null", updated.ExpiresAt)
	}
}

// Scenario: Share Recipient Cannot Set Expiry
// WHEN a user whose only relationship to a link is a link_shares record
// attempts to set or clear expires_at THEN 403 Forbidden and no modification.
func TestAPILifecycle_ShareRecipientCannotSetExpiry(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	recipient := seedUser(t, env, "recipient@example.com", "user")
	ownerTok := seedToken(t, env, owner.ID)
	recipientTok := seedToken(t, env, recipient.ID)

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	var link linkJSON
	if rec := doJSON(t, env, ownerTok, http.MethodPost, "/links",
		`{"slug":"shared","url":"https://example.com","visibility":"secure","expires_at":"`+future.Format(time.RFC3339)+`"}`, &link); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}
	if err := env.LinkStore.AddShare(t.Context(), link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	// Attempt to move the expiry out.
	rec := doJSON(t, env, recipientTok, http.MethodPut, "/links/"+link.ID,
		`{"url":"https://example.com","expires_at":"`+future.Add(24*time.Hour).Format(time.RFC3339)+`"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("recipient set expiry status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// Attempt to clear it.
	rec = doJSON(t, env, recipientTok, http.MethodPut, "/links/"+link.ID,
		`{"url":"https://example.com","expires_at":null}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("recipient clear expiry status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// The stored value is untouched.
	var got linkJSON
	doJSON(t, env, ownerTok, http.MethodGet, "/links/"+link.ID, "", &got)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(future) {
		t.Errorf("expires_at after forbidden writes = %v, want %v", got.ExpiresAt, future)
	}
}
