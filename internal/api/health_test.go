// REST API tests for the SPEC-0020 health slice deferred from PR #290:
// health_checks_disabled accepted on POST and PUT under CanEdit and
// serialized (with the health object) only to capable callers, and the
// derived health state — including the surfacing rule — flowing through
// GET /api/v1/links responses.
//
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP", REQ "Destination Health Checking", ADR-0020
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// healthLinkJSON is the subset of LinkResponse these tests assert on.
type healthLinkJSON struct {
	ID     string `json:"id"`
	Slug   string `json:"slug"`
	Title  string `json:"title"`
	Health *struct {
		Status        string     `json:"status"`
		LastCheckedAt *time.Time `json:"last_checked_at"`
		LastStatus    *int       `json:"last_status"`
	} `json:"health"`
	HealthChecksDisabled *bool    `json:"health_checks_disabled"`
	Tags                 []string `json:"tags"`
}

// seedBrokenHealth records three consecutive failed probes so the derived
// state is broken.
func seedBrokenHealth(t *testing.T, env *testEnv, linkID string) {
	t.Helper()
	status := 503
	for i := 0; i < 3; i++ {
		if _, err := env.LinkStore.RecordHealthFailure(context.Background(), linkID, &status, "unavailable", time.Now().UTC().Truncate(time.Second), time.Hour); err != nil {
			t.Fatalf("seed failure: %v", err)
		}
	}
}

// POST /api/v1/links accepts the optional health_checks_disabled flag: the
// creator is the primary owner, so CanEdit is held by construction.
func TestAPIHealth_PostAcceptsHealthChecksDisabled(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	var created healthLinkJSON
	rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"quiet","url":"https://example.com","health_checks_disabled":true}`, &created)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if created.HealthChecksDisabled == nil || !*created.HealthChecksDisabled {
		t.Errorf("health_checks_disabled = %v, want true on the create response", created.HealthChecksDisabled)
	}

	// Omitting the flag yields the FALSE default.
	var plain healthLinkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"loud","url":"https://example.com/2"}`, &plain); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	if plain.HealthChecksDisabled == nil || *plain.HealthChecksDisabled {
		t.Errorf("health_checks_disabled = %v, want default false", plain.HealthChecksDisabled)
	}
}

// PUT /api/v1/links/{id} accepts health_checks_disabled — both as a
// toggle-only body (no other fields clobbered) and alongside a full update.
func TestAPIHealth_PutAcceptsHealthChecksDisabled(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	var created healthLinkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"toggle-health","url":"https://example.com","title":"Keep Me","tags":["ops"]}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}

	// Toggle-only body: flips the flag without running the full-resource
	// update (title and tags survive).
	var toggled healthLinkJSON
	rec := doJSON(t, env, token, http.MethodPut, "/links/"+created.ID, `{"health_checks_disabled":true}`, &toggled)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if toggled.HealthChecksDisabled == nil || !*toggled.HealthChecksDisabled {
		t.Errorf("health_checks_disabled = %v, want true after the toggle", toggled.HealthChecksDisabled)
	}
	if toggled.Title != "Keep Me" || len(toggled.Tags) != 1 {
		t.Errorf("toggle-only body clobbered siblings: title=%q tags=%v", toggled.Title, toggled.Tags)
	}

	// Alongside a full update.
	var full healthLinkJSON
	rec = doJSON(t, env, token, http.MethodPut, "/links/"+created.ID,
		`{"url":"https://example.com/moved","title":"Renamed","health_checks_disabled":false}`, &full)
	if rec.Code != http.StatusOK {
		t.Fatalf("full update status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if full.HealthChecksDisabled == nil || *full.HealthChecksDisabled {
		t.Errorf("health_checks_disabled = %v, want false after the full update", full.HealthChecksDisabled)
	}
	if full.Title != "Renamed" {
		t.Errorf("title = %q, want %q", full.Title, "Renamed")
	}
}

// The opt-out is an edit: a share recipient (CanView/CanStats but not
// CanEdit) must get 403 and the flag must not change.
func TestAPIHealth_ShareRecipientCannotToggleOptOut(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	recipient := seedUser(t, env, "recipient@example.com", "user")
	ownerTok := seedToken(t, env, owner.ID)
	recipientTok := seedToken(t, env, recipient.ID)

	var created healthLinkJSON
	if rec := doJSON(t, env, ownerTok, http.MethodPost, "/links",
		`{"slug":"shared-health","url":"https://example.com","visibility":"secure"}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}
	if err := env.LinkStore.AddShare(context.Background(), created.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	rec := doJSON(t, env, recipientTok, http.MethodPut, "/links/"+created.ID, `{"health_checks_disabled":true}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("recipient toggle status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	var after healthLinkJSON
	if rec := doJSON(t, env, ownerTok, http.MethodGet, "/links/"+created.ID, "", &after); rec.Code != http.StatusOK {
		t.Fatalf("owner get status = %d", rec.Code)
	}
	if after.HealthChecksDisabled == nil || *after.HealthChecksDisabled {
		t.Errorf("health_checks_disabled changed by a share recipient: %v", after.HealthChecksDisabled)
	}
}

// Derived health flows through GET: three failed checks surface as broken
// with the recorded details for a capable caller.
func TestAPIHealth_BrokenStateSurfacesToCapableCaller(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	var created healthLinkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"rotting","url":"https://example.com"}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}
	seedBrokenHealth(t, env, created.ID)

	var got healthLinkJSON
	if rec := doJSON(t, env, token, http.MethodGet, "/links/"+created.ID, "", &got); rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	if got.Health == nil {
		t.Fatal("health object missing for the owner")
	}
	if got.Health.Status != "broken" {
		t.Errorf("health.status = %q, want %q", got.Health.Status, "broken")
	}
	if got.Health.LastStatus == nil || *got.Health.LastStatus != 503 {
		t.Errorf("health.last_status = %v, want 503", got.Health.LastStatus)
	}
	if got.Health.LastCheckedAt == nil {
		t.Error("health.last_checked_at missing")
	}
}

// Scenario: Opt-Out Honored (REST half)
// WHEN an owner sets the health-check opt-out THEN REST health.status
// reports "unchecked" with null details — the frozen link_health row is no
// longer surfaced.
func TestAPIHealth_OptOutHonored(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	token := seedToken(t, env, owner.ID)

	var created healthLinkJSON
	if rec := doJSON(t, env, token, http.MethodPost, "/links",
		`{"slug":"muted","url":"https://example.com"}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}
	seedBrokenHealth(t, env, created.ID)

	var toggled healthLinkJSON
	if rec := doJSON(t, env, token, http.MethodPut, "/links/"+created.ID, `{"health_checks_disabled":true}`, &toggled); rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d, want 200", rec.Code)
	}
	if toggled.Health == nil || toggled.Health.Status != "unchecked" {
		t.Fatalf("health = %+v, want status unchecked after opting out", toggled.Health)
	}
	if toggled.Health.LastCheckedAt != nil || toggled.Health.LastStatus != nil {
		t.Errorf("health details = (%v, %v), want nulls (frozen row not surfaced)",
			toggled.Health.LastCheckedAt, toggled.Health.LastStatus)
	}
}

// Callers without capabilities receive neither the health object nor the
// health_checks_disabled flag — both are capability-gated.
func TestAPIHealth_NonCapableCallerGetsNoHealthFields(t *testing.T) {
	env := newTestEnv(t)
	owner := seedUser(t, env, "owner@example.com", "user")
	stranger := seedUser(t, env, "stranger@example.com", "user")
	ownerTok := seedToken(t, env, owner.ID)
	strangerTok := seedToken(t, env, stranger.ID)

	var created healthLinkJSON
	if rec := doJSON(t, env, ownerTok, http.MethodPost, "/links",
		`{"slug":"open-wide","url":"https://example.com/open","visibility":"public"}`, &created); rec.Code != http.StatusCreated {
		t.Fatalf("seed link: %d", rec.Code)
	}
	seedBrokenHealth(t, env, created.ID)

	rec := doJSON(t, env, strangerTok, http.MethodGet, "/links?url="+"https%3A%2F%2Fexample.com%2Fopen", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stranger list status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Links []healthLinkJSON `json:"links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Links) != 1 {
		t.Fatalf("stranger list returned %d links, want 1", len(list.Links))
	}
	if list.Links[0].Health != nil {
		t.Errorf("health object leaked to a non-capable caller: %+v", list.Links[0].Health)
	}
	if list.Links[0].HealthChecksDisabled != nil {
		t.Errorf("health_checks_disabled leaked to a non-capable caller: %v", *list.Links[0].HealthChecksDisabled)
	}
	if strings.Contains(rec.Body.String(), "health_checks_disabled") {
		t.Error("raw body carries health_checks_disabled for a non-capable caller")
	}
}
