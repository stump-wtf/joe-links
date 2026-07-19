// REST API tests for the SPEC-0020 REQ "Lifecycle State in API and MCP"
// scenarios: lifecycle fields on link resources, the capability-gated health
// object, and the archived round-trip on PUT /links/{id}. Tests are named
// after the spec scenarios so the spec↔test mapping is auditable.
//
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP", REQ "Archive State", ADR-0020
package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// lifecycleLinkJSON is the subset of LinkResponse these tests assert on.
type lifecycleLinkJSON struct {
	ID             string     `json:"id"`
	Slug           string     `json:"slug"`
	Title          string     `json:"title"`
	ExpiresAt      *time.Time `json:"expires_at"`
	ArchivedAt     *time.Time `json:"archived_at"`
	LifecycleState string     `json:"lifecycle_state"`
	Health         *struct {
		Status        string     `json:"status"`
		LastCheckedAt *time.Time `json:"last_checked_at"`
		LastStatus    *int       `json:"last_status"`
	} `json:"health"`
	Tags []string `json:"tags"`
}

type lifecycleListJSON struct {
	Links []lifecycleLinkJSON `json:"links"`
}

// Scenario: API Response Carries Lifecycle State
// WHEN GET /api/v1/links/{id} is called by an owner for a link with
// expires_at in the past THEN the response includes the stored expires_at,
// "archived_at": null, and "lifecycle_state": "expired".
func TestAPILifecycle_APIResponseCarriesLifecycleState(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	var created lifecycleLinkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"was-temporary","url":"https://example.com"}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}
	past := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	backdateExpiryAPI(t, env, created.ID, past)

	var got lifecycleLinkJSON
	rec := doJSON(t, env, token, http.MethodGet, "/links/"+created.ID, "", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(past) {
		t.Errorf("expires_at = %v, want stored %v", got.ExpiresAt, past)
	}
	if got.ArchivedAt != nil {
		t.Errorf("archived_at = %v, want null", got.ArchivedAt)
	}
	if got.LifecycleState != "expired" {
		t.Errorf("lifecycle_state = %q, want %q", got.LifecycleState, "expired")
	}
	// The raw JSON must carry archived_at explicitly as null, not omit it.
	if !strings.Contains(rec.Body.String(), `"archived_at":null`) {
		t.Errorf(`response must serialize "archived_at": null; body=%s`, rec.Body.String())
	}
}

// Scenario: API Archive Round-Trip
// WHEN an owner calls PUT /api/v1/links/{id} with {"archived": true} and
// later with {"archived": false} THEN the first call sets archived_at and the
// second clears it, with lifecycle_state tracking accordingly.
func TestAPILifecycle_APIArchiveRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	var created lifecycleLinkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"toggle-me","url":"https://example.com","title":"Keep Me","tags":["ops"]}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}

	var archived lifecycleLinkJSON
	rec := doJSON(t, env, token, http.MethodPut, "/links/"+created.ID, `{"archived":true}`, &archived)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if archived.ArchivedAt == nil {
		t.Fatal("archived_at not set after {\"archived\": true}")
	}
	if archived.LifecycleState != "archived" {
		t.Errorf("lifecycle_state = %q, want %q", archived.LifecycleState, "archived")
	}
	// The archive-only body must not run the full-resource update: title and
	// tags survive untouched.
	if archived.Title != "Keep Me" {
		t.Errorf("title after archive-only PUT = %q, want %q", archived.Title, "Keep Me")
	}
	if len(archived.Tags) != 1 || archived.Tags[0] != "ops" {
		t.Errorf("tags after archive-only PUT = %v, want [ops]", archived.Tags)
	}

	// Re-archiving is idempotent: the original archived_at is kept.
	var again lifecycleLinkJSON
	if rec := doJSON(t, env, token, http.MethodPut, "/links/"+created.ID, `{"archived":true}`, &again); rec.Code != http.StatusOK {
		t.Fatalf("re-archive status = %d, want 200", rec.Code)
	}
	if again.ArchivedAt == nil || !again.ArchivedAt.Equal(*archived.ArchivedAt) {
		t.Errorf("archived_at after re-archive = %v, want unchanged %v", again.ArchivedAt, archived.ArchivedAt)
	}

	var restored lifecycleLinkJSON
	rec = doJSON(t, env, token, http.MethodPut, "/links/"+created.ID, `{"archived":false}`, &restored)
	if rec.Code != http.StatusOK {
		t.Fatalf("unarchive status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if restored.ArchivedAt != nil {
		t.Errorf("archived_at after {\"archived\": false} = %v, want null", restored.ArchivedAt)
	}
	if restored.LifecycleState != "active" {
		t.Errorf("lifecycle_state = %q, want %q", restored.LifecycleState, "active")
	}
}

// The archive-only PUT carve-out fires only when "archived" is the sole
// populated field. Sibling fields without url must not be silently discarded:
// {"archived": true, "expires_at": <past>} is a validation error exactly as
// MCP update_link treats it (REST/MCP parity), and {"archived": true,
// "title": ...} keeps the historical "url is required" contract.
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP", REQ "Link
// Expiration" scenario "Past Expiration Rejected"
func TestAPILifecycle_ArchiveWithSiblingFieldsRequiresURL(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	var created lifecycleLinkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"stay-whole","url":"https://example.com","title":"Keep Me"}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	cases := []struct{ name, body string }{
		{"past expires_at sibling", `{"archived":true,"expires_at":"` + past + `"}`},
		{"title sibling", `{"archived":true,"title":"Renamed"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, env, token, http.MethodPut, "/links/"+created.ID, tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// Nothing from the rejected bodies was applied.
	var got lifecycleLinkJSON
	if rec := doJSON(t, env, token, http.MethodGet, "/links/"+created.ID, "", &got); rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	if got.ArchivedAt != nil || got.LifecycleState != "active" {
		t.Errorf("rejected body changed archive state: archived_at=%v lifecycle_state=%q", got.ArchivedAt, got.LifecycleState)
	}
	if got.ExpiresAt != nil {
		t.Errorf("rejected body persisted expires_at = %v, want nil", got.ExpiresAt)
	}
	if got.Title != "Keep Me" {
		t.Errorf("rejected body changed title = %q, want %q", got.Title, "Keep Me")
	}
}

// Scenario: Non-Capable Caller Gets No Health Data
// WHEN an authenticated API caller with no ownership, share, or admin
// relationship retrieves a public link THEN the response includes the
// lifecycle fields but not the health object; capable callers do receive it.
func TestAPILifecycle_NonCapableCallerGetsNoHealthData(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	stranger := seedUser(t, env, "stranger@example.com", "user")
	ownerTok := seedToken(t, env, owner.ID)
	strangerTok := seedToken(t, env, stranger.ID)

	var created lifecycleLinkJSON
	if rec := doJSON(t, env, ownerTok, http.MethodPost, "/links",
		`{"slug":"open-to-all","url":"https://example.com/public","visibility":"public"}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}

	// The stranger reaches the public link through the ?url= lookup (the
	// direct GET /links/{id} is capability-gated and would 403).
	rec := doJSON(t, env, strangerTok, http.MethodGet, "/links?url="+"https%3A%2F%2Fexample.com%2Fpublic", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stranger list status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list lifecycleListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Links) != 1 {
		t.Fatalf("stranger list returned %d links, want 1", len(list.Links))
	}
	got := list.Links[0]
	if got.LifecycleState != "active" {
		t.Errorf("lifecycle_state = %q, want %q (lifecycle fields are not capability-gated)", got.LifecycleState, "active")
	}
	if got.Health != nil {
		t.Errorf("health object leaked to a non-capable caller: %+v", got.Health)
	}

	// The owner receives the health object — status "unchecked" with null
	// details until the health checker story lands.
	var mine lifecycleLinkJSON
	if rec := doJSON(t, env, ownerTok, http.MethodGet, "/links/"+created.ID, "", &mine); rec.Code != http.StatusOK {
		t.Fatalf("owner get status = %d, want 200", rec.Code)
	}
	if mine.Health == nil {
		t.Fatal("health object missing for the owner")
	}
	if mine.Health.Status != "unchecked" || mine.Health.LastCheckedAt != nil || mine.Health.LastStatus != nil {
		t.Errorf("health = %+v, want status unchecked with null details", mine.Health)
	}
}
