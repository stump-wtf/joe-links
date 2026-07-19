// MCP tool tests for the SPEC-0020 REQ "Link Expiration" scenarios:
// create_link/update_link accept expires_at with validation identical to the
// REST API (past rejected, round-trip accepted, empty string clears), and
// capability gating keeps share recipients away from expiry. Tests are named
// after the spec scenarios so the spec↔test mapping is auditable.
//
// Governing: SPEC-0020 REQ "Link Expiration", SPEC-0018 REQ "Authorization
// Parity with the REST API", ADR-0020
package mcp_test

import (
	"context"
	"testing"
	"time"
)

// structuredExpiresAt extracts the expires_at field from a tool result's
// structured content; ok is false when the field is absent (never expires).
func structuredExpiresAt(t *testing.T, resp *toolResponse) (time.Time, bool) {
	t.Helper()
	raw, present := resp.Structured["expires_at"]
	if !present || raw == nil {
		return time.Time{}, false
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("expires_at is %T, want RFC 3339 string", raw)
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse expires_at %q: %v", s, err)
	}
	return parsed, true
}

// Scenario: Link Created with Expiration
// WHEN create_link is called with a future expires_at
// THEN the link is persisted with it and get_link reports it back.
func TestMCPLifecycle_LinkCreatedWithExpiration(t *testing.T) {
	env := newFullEnv(t, nil)
	_, token := seedUserToken(t, env, "agent@example.com")

	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	created := callTool(t, env, token, "create_link", map[string]any{
		"slug": "expires-mcp", "url": "https://example.com",
		"expires_at": future.Format(time.RFC3339),
	})
	if created.IsError {
		t.Fatalf("create_link: %s %s", created.ErrCode, created.ErrMessage)
	}
	if got, ok := structuredExpiresAt(t, created); !ok || !got.Equal(future) {
		t.Errorf("create_link expires_at = %v (present=%v), want %v", got, ok, future)
	}

	fetched := callTool(t, env, token, "get_link", map[string]any{"link": "expires-mcp"})
	if fetched.IsError {
		t.Fatalf("get_link: %s %s", fetched.ErrCode, fetched.ErrMessage)
	}
	if got, ok := structuredExpiresAt(t, fetched); !ok || !got.Equal(future) {
		t.Errorf("get_link expires_at = %v (present=%v), want %v", got, ok, future)
	}

	// Omitting expires_at yields a link that never expires.
	plain := callTool(t, env, token, "create_link", map[string]any{
		"slug": "forever-mcp", "url": "https://example.com/2",
	})
	if plain.IsError {
		t.Fatalf("create_link: %s %s", plain.ErrCode, plain.ErrMessage)
	}
	if _, ok := structuredExpiresAt(t, plain); ok {
		t.Error("expires_at present on a link created without one")
	}
}

// Scenario: Past Expiration Rejected
// WHEN create_link or update_link passes a past expires_at that differs from
// the stored value THEN validation_failed and nothing is persisted.
func TestMCPLifecycle_PastExpirationRejected(t *testing.T) {
	env := newFullEnv(t, nil)
	_, token := seedUserToken(t, env, "agent@example.com")
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	created := callTool(t, env, token, "create_link", map[string]any{
		"slug": "born-dead-mcp", "url": "https://example.com", "expires_at": past,
	})
	if !created.IsError || created.ErrCode != "validation_failed" {
		t.Fatalf("create_link past expiry: isError=%v code=%s, want validation_failed", created.IsError, created.ErrCode)
	}
	if _, err := env.LinkStore.GetBySlug(ctx, "born-dead-mcp"); err == nil {
		t.Error("link was created despite past expiration")
	}

	seed := callTool(t, env, token, "create_link", map[string]any{"slug": "alive-mcp", "url": "https://example.com"})
	if seed.IsError {
		t.Fatalf("seed create_link: %s", seed.ErrMessage)
	}
	updated := callTool(t, env, token, "update_link", map[string]any{"link": "alive-mcp", "expires_at": past})
	if !updated.IsError || updated.ErrCode != "validation_failed" {
		t.Fatalf("update_link past expiry: isError=%v code=%s, want validation_failed", updated.IsError, updated.ErrCode)
	}
	got, err := env.LinkStore.GetBySlug(ctx, "alive-mcp")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after rejected update = %v, want nil", got.ExpiresAt)
	}

	// Bad wire format is a validation error too, not an internal one.
	garbled := callTool(t, env, token, "update_link", map[string]any{"link": "alive-mcp", "expires_at": "next tuesday"})
	if !garbled.IsError || garbled.ErrCode != "validation_failed" {
		t.Errorf("update_link garbage expiry: isError=%v code=%s, want validation_failed", garbled.IsError, garbled.ErrCode)
	}
}

// Scenario: Expired Link Stays Editable
// WHEN update_link round-trips the stored (past) expires_at while changing the
// title THEN the update succeeds and expires_at is unchanged.
func TestMCPLifecycle_ExpiredLinkStaysEditable(t *testing.T) {
	env := newFullEnv(t, nil)
	_, token := seedUserToken(t, env, "agent@example.com")
	ctx := context.Background()

	seed := callTool(t, env, token, "create_link", map[string]any{"slug": "expired-mcp", "url": "https://example.com", "title": "Old"})
	if seed.IsError {
		t.Fatalf("seed create_link: %s", seed.ErrMessage)
	}
	link, err := env.LinkStore.GetBySlug(ctx, "expired-mcp")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	past := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	if _, err := env.DB.Exec(env.DB.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`), past, link.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}

	// Round-trip the stored past value, as a full-resource client would.
	updated := callTool(t, env, token, "update_link", map[string]any{
		"link": "expired-mcp", "title": "New", "expires_at": past.Format(time.RFC3339),
	})
	if updated.IsError {
		t.Fatalf("update_link round-trip: %s %s", updated.ErrCode, updated.ErrMessage)
	}
	if got, ok := structuredExpiresAt(t, updated); !ok || !got.Equal(past) {
		t.Errorf("expires_at = %v (present=%v), want unchanged %v", got, ok, past)
	}

	// Omitting expires_at entirely also leaves the stored value unchanged.
	omitted := callTool(t, env, token, "update_link", map[string]any{"link": "expired-mcp", "title": "Newer"})
	if omitted.IsError {
		t.Fatalf("update_link omitted expiry: %s %s", omitted.ErrCode, omitted.ErrMessage)
	}
	if got, ok := structuredExpiresAt(t, omitted); !ok || !got.Equal(past) {
		t.Errorf("expires_at after omitted field = %v (present=%v), want unchanged %v", got, ok, past)
	}
}

// Scenario: Expiration Cleared on Edit
// WHEN update_link passes an empty expires_at THEN the stored value becomes
// NULL and the link no longer expires.
func TestMCPLifecycle_ExpirationClearedOnEdit(t *testing.T) {
	env := newFullEnv(t, nil)
	_, token := seedUserToken(t, env, "agent@example.com")
	ctx := context.Background()

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	seed := callTool(t, env, token, "create_link", map[string]any{
		"slug": "clear-mcp", "url": "https://example.com", "expires_at": future.Format(time.RFC3339),
	})
	if seed.IsError {
		t.Fatalf("seed create_link: %s", seed.ErrMessage)
	}

	cleared := callTool(t, env, token, "update_link", map[string]any{"link": "clear-mcp", "expires_at": ""})
	if cleared.IsError {
		t.Fatalf("update_link clear: %s %s", cleared.ErrCode, cleared.ErrMessage)
	}
	if _, ok := structuredExpiresAt(t, cleared); ok {
		t.Error("expires_at still present after clearing")
	}
	got, err := env.LinkStore.GetBySlug(ctx, "clear-mcp")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("stored expires_at after clear = %v, want nil", got.ExpiresAt)
	}
}

// Scenario: Non-Capable Caller Gets No Health Data (MCP surface — the REST
// half lives in internal/api/lifecycle_archive_test.go)
// WHEN a caller with no ownership, share, or admin relationship lists public
// links THEN a stranger's public row carries lifecycle_state but no health
// object, while the caller's own row in the same listing includes it.
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
func TestMCPLifecycle_NonCapableCallerGetsNoHealthData(t *testing.T) {
	env := newFullEnv(t, nil)
	_, ownerTok := seedUserToken(t, env, "owner@example.com")
	_, strangerTok := seedUserToken(t, env, "stranger@example.com")

	if resp := callTool(t, env, ownerTok, "create_link", map[string]any{
		"slug": "open-to-all", "url": "https://example.com/public", "visibility": "public",
	}); resp.IsError {
		t.Fatalf("seed create_link: %s", resp.ErrMessage)
	}
	if resp := callTool(t, env, strangerTok, "create_link", map[string]any{
		"slug": "strangers-own", "url": "https://example.com/mine", "visibility": "public",
	}); resp.IsError {
		t.Fatalf("seed create_link: %s", resp.ErrMessage)
	}

	resp := callTool(t, env, strangerTok, "list_links", map[string]any{"filter": "public"})
	if resp.IsError {
		t.Fatalf("list_links: %s %s", resp.ErrCode, resp.ErrMessage)
	}
	links, _ := resp.Structured["links"].([]any)
	rows := map[string]map[string]any{}
	for _, l := range links {
		m, _ := l.(map[string]any)
		rows[m["slug"].(string)] = m
	}

	theirs, ok := rows["open-to-all"]
	if !ok {
		t.Fatalf("public list missing open-to-all; rows=%v", rows)
	}
	if got := theirs["lifecycle_state"]; got != "active" {
		t.Errorf("lifecycle_state = %v, want active (lifecycle fields are not capability-gated)", got)
	}
	if h, present := theirs["health"]; present {
		t.Errorf("health object leaked to a non-capable caller: %v", h)
	}

	mine, ok := rows["strangers-own"]
	if !ok {
		t.Fatalf("public list missing strangers-own; rows=%v", rows)
	}
	if mine["health"] == nil {
		t.Error("health object missing on the caller's own public row")
	}
}

// Scenario: Share Recipient Cannot Set Expiry
// WHEN a user whose only relationship to the link is a link_shares record
// calls update_link with expires_at THEN forbidden and no modification.
func TestMCPLifecycle_ShareRecipientCannotSetExpiry(t *testing.T) {
	env := newFullEnv(t, nil)
	owner, ownerTok := seedUserToken(t, env, "owner@example.com")
	recipient, recipientTok := seedUserToken(t, env, "recipient@example.com")
	ctx := context.Background()

	seed := callTool(t, env, ownerTok, "create_link", map[string]any{
		"slug": "shared-mcp", "url": "https://example.com", "visibility": "secure",
	})
	if seed.IsError {
		t.Fatalf("seed create_link: %s", seed.ErrMessage)
	}
	link, err := env.LinkStore.GetBySlug(ctx, "shared-mcp")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if err := env.LinkStore.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	denied := callTool(t, env, recipientTok, "update_link", map[string]any{"link": "shared-mcp", "expires_at": future})
	if !denied.IsError || denied.ErrCode != "forbidden" {
		t.Fatalf("recipient set expiry: isError=%v code=%s, want forbidden", denied.IsError, denied.ErrCode)
	}
	deniedClear := callTool(t, env, recipientTok, "update_link", map[string]any{"link": "shared-mcp", "expires_at": ""})
	if !deniedClear.IsError || deniedClear.ErrCode != "forbidden" {
		t.Fatalf("recipient clear expiry: isError=%v code=%s, want forbidden", deniedClear.IsError, deniedClear.ErrCode)
	}

	got, err := env.LinkStore.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after forbidden writes = %v, want nil", got.ExpiresAt)
	}
}
