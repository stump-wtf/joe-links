// MCP tests for the SPEC-0020 health slice deferred from PR #290:
// health_checks_disabled accepted by create_link/update_link with REST-equal
// semantics, health fields derived from the link_health table with the
// surfacing rule, byte-for-byte health parity with REST for a genuinely
// broken link, and the lifecycle exclusion of expired/archived links from
// list_links filter=public.
//
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP", REQ "Destination
// Health Checking", REQ "Health Badges and Admin Report", ADR-0020
package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// seedBrokenHealthMCP records three consecutive failed probes so the derived
// state is broken.
func seedBrokenHealthMCP(t *testing.T, env *fullEnv, linkID string) {
	t.Helper()
	status := 503
	for i := 0; i < 3; i++ {
		if _, err := env.LinkStore.RecordHealthFailure(context.Background(), linkID, &status, "unavailable", time.Now().UTC().Truncate(time.Second), time.Hour); err != nil {
			t.Fatalf("seed failure: %v", err)
		}
	}
}

// create_link and update_link accept health_checks_disabled with the same
// semantics as POST/PUT /api/v1/links: create defaults to false, the flag
// round-trips on payloads, and omitting it on update leaves it unchanged.
func TestMCPHealth_CreateAndUpdateAcceptHealthChecksDisabled(t *testing.T) {
	env := newFullEnv(t, nil)
	_, tok := seedUserToken(t, env, "owner@example.com")

	created := callTool(t, env, tok, "create_link", map[string]any{
		"slug": "quiet", "url": "https://example.com", "health_checks_disabled": true,
	})
	if created.IsError {
		t.Fatalf("create_link: %s %s", created.ErrCode, created.ErrMessage)
	}
	if got := created.Structured["health_checks_disabled"]; got != true {
		t.Errorf("create payload health_checks_disabled = %v, want true", got)
	}

	// Omitted on update: unchanged.
	updated := callTool(t, env, tok, "update_link", map[string]any{"link": "quiet", "title": "Still Quiet"})
	if updated.IsError {
		t.Fatalf("update_link: %s %s", updated.ErrCode, updated.ErrMessage)
	}
	if got := updated.Structured["health_checks_disabled"]; got != true {
		t.Errorf("omitted flag changed the stored value: %v", got)
	}

	// Explicit false: opts back in.
	reenabled := callTool(t, env, tok, "update_link", map[string]any{"link": "quiet", "health_checks_disabled": false})
	if reenabled.IsError {
		t.Fatalf("update_link: %s %s", reenabled.ErrCode, reenabled.ErrMessage)
	}
	if got := reenabled.Structured["health_checks_disabled"]; got != false {
		t.Errorf("health_checks_disabled = %v, want false after opting back in", got)
	}
}

// The opt-out is an edit: a share recipient must be refused and the flag
// must not change — matching REST's 403.
func TestMCPHealth_ShareRecipientCannotToggleOptOut(t *testing.T) {
	env := newFullEnv(t, nil)
	owner, ownerTok := seedUserToken(t, env, "owner@example.com")
	recipient, recipientTok := seedUserToken(t, env, "recipient@example.com")

	ctx := context.Background()
	link, err := env.LinkStore.CreateFull(ctx, "shared-health", "https://example.com",
		owner.ID, "", "", "secure", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := env.LinkStore.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	resp := callTool(t, env, recipientTok, "update_link", map[string]any{"link": link.ID, "health_checks_disabled": true})
	if !resp.IsError {
		t.Fatal("share recipient toggled the health-check opt-out, want forbidden")
	}

	after := callTool(t, env, ownerTok, "get_link", map[string]any{"link": link.ID})
	if after.IsError {
		t.Fatalf("get_link: %s %s", after.ErrCode, after.ErrMessage)
	}
	if got := after.Structured["health_checks_disabled"]; got != false {
		t.Errorf("health_checks_disabled changed by a share recipient: %v", got)
	}
}

// Scenario: MCP Parity (broken-link half)
// A genuinely broken active link reports the same health object — status,
// last_checked_at, last_status — and the same health_checks_disabled flag on
// MCP get_link and REST GET /links/{id}.
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP" scenario "MCP Parity"
func TestMCPHealth_BrokenHealthParityWithREST(t *testing.T) {
	env := newParityEnv(t)
	owner, ownerTok := seedUserToken(t, env.fullEnv, "owner@example.com")

	link, err := env.LinkStore.CreateFull(context.Background(), "rotting", "https://example.com/rotting",
		owner.ID, "", "", "private", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	seedBrokenHealthMCP(t, env.fullEnv, link.ID)

	mcpResp := callTool(t, env.fullEnv, ownerTok, "get_link", map[string]any{"link": link.ID})
	if mcpResp.IsError {
		t.Fatalf("get_link: %s %s", mcpResp.ErrCode, mcpResp.ErrMessage)
	}
	health, _ := mcpResp.Structured["health"].(map[string]any)
	if health == nil {
		t.Fatal("MCP health object missing for the owner")
	}
	if got := health["status"]; got != "broken" {
		t.Errorf("MCP health.status = %v, want broken", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/links/"+link.ID, nil)
	req.Header.Set("Authorization", "Bearer "+ownerTok)
	rec := httptest.NewRecorder()
	env.REST.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST get status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var restBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &restBody); err != nil {
		t.Fatalf("decode REST body: %v", err)
	}
	if !reflect.DeepEqual(mcpResp.Structured["health"], restBody["health"]) {
		t.Errorf("PARITY VIOLATION: health object diverges — MCP=%v REST=%v",
			mcpResp.Structured["health"], restBody["health"])
	}
	if !reflect.DeepEqual(mcpResp.Structured["health_checks_disabled"], restBody["health_checks_disabled"]) {
		t.Errorf("PARITY VIOLATION: health_checks_disabled diverges — MCP=%v REST=%v",
			mcpResp.Structured["health_checks_disabled"], restBody["health_checks_disabled"])
	}
}

// Scenario: Opt-Out Honored (MCP half)
// WHEN an owner sets the opt-out via update_link THEN health.status reports
// "unchecked" with null details — the frozen link_health row is no longer
// surfaced.
func TestMCPHealth_OptOutHonored(t *testing.T) {
	env := newFullEnv(t, nil)
	owner, tok := seedUserToken(t, env, "owner@example.com")

	link, err := env.LinkStore.CreateFull(context.Background(), "muted", "https://example.com/muted",
		owner.ID, "", "", "private", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	seedBrokenHealthMCP(t, env, link.ID)

	resp := callTool(t, env, tok, "update_link", map[string]any{"link": link.ID, "health_checks_disabled": true})
	if resp.IsError {
		t.Fatalf("update_link: %s %s", resp.ErrCode, resp.ErrMessage)
	}
	health, _ := resp.Structured["health"].(map[string]any)
	if health == nil {
		t.Fatal("health object missing for the owner")
	}
	if got := health["status"]; got != "unchecked" {
		t.Errorf("health.status = %v, want unchecked after opting out", got)
	}
	if health["last_checked_at"] != nil || health["last_status"] != nil {
		t.Errorf("health details = (%v, %v), want nulls (frozen row not surfaced)",
			health["last_checked_at"], health["last_status"])
	}
}

// PR #290 debt, item 2 (MCP half): list_links filter=public must not
// enumerate expired or archived public links — the lifecycle predicate lives
// in the shared ListPublic query.
// Governing: SPEC-0020 REQ "Health Badges and Admin Report" scenario "Public
// Browser Shows No Health Data"; Security "Resolution Ordering and Oracle Resistance"
func TestMCPHealth_PublicFilterExcludesExpiredAndArchived(t *testing.T) {
	env := newFullEnv(t, nil)
	owner, _ := seedUserToken(t, env, "owner@example.com")
	_, strangerTok := seedUserToken(t, env, "stranger@example.com")

	ctx := context.Background()
	if _, err := env.LinkStore.CreateFull(ctx, "public-live", "https://example.com/live",
		owner.ID, "", "", "public", nil, nil, nil, ""); err != nil {
		t.Fatalf("seed live link: %v", err)
	}
	expired, err := env.LinkStore.CreateFull(ctx, "public-expired", "https://example.com/expired",
		owner.ID, "", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed expired link: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := env.DB.Exec(env.DB.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`), past, expired.ID); err != nil {
		t.Fatalf("expire link: %v", err)
	}
	archived, err := env.LinkStore.CreateFull(ctx, "public-archived", "https://example.com/archived",
		owner.ID, "", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed archived link: %v", err)
	}
	if _, err := env.LinkStore.SetArchived(ctx, archived.ID, true); err != nil {
		t.Fatalf("archive link: %v", err)
	}

	resp := callTool(t, env, strangerTok, "list_links", map[string]any{"filter": "public"})
	if resp.IsError {
		t.Fatalf("list_links: %s %s", resp.ErrCode, resp.ErrMessage)
	}
	links, _ := resp.Structured["links"].([]any)
	slugs := map[string]bool{}
	for _, l := range links {
		m, _ := l.(map[string]any)
		slugs[m["slug"].(string)] = true
	}
	if !slugs["public-live"] {
		t.Errorf("public filter missing the live link; slugs=%v", slugs)
	}
	if slugs["public-expired"] || slugs["public-archived"] {
		t.Errorf("expired/archived public links enumerable via list_links filter=public; slugs=%v", slugs)
	}
}

// Callers without capabilities receive neither the health object nor the
// health_checks_disabled flag on list rows — both are capability-gated,
// matching REST.
func TestMCPHealth_NonCapableCallerGetsNoHealthFields(t *testing.T) {
	env := newFullEnv(t, nil)
	owner, _ := seedUserToken(t, env, "owner@example.com")
	_, strangerTok := seedUserToken(t, env, "stranger@example.com")

	link, err := env.LinkStore.CreateFull(context.Background(), "open-wide", "https://example.com/open",
		owner.ID, "", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	seedBrokenHealthMCP(t, env, link.ID)

	resp := callTool(t, env, strangerTok, "list_links", map[string]any{"filter": "public"})
	if resp.IsError {
		t.Fatalf("list_links: %s %s", resp.ErrCode, resp.ErrMessage)
	}
	links, _ := resp.Structured["links"].([]any)
	for _, l := range links {
		m, _ := l.(map[string]any)
		if m["slug"] != "open-wide" {
			continue
		}
		if h, present := m["health"]; present {
			t.Errorf("health object leaked to a non-capable caller: %v", h)
		}
		if f, present := m["health_checks_disabled"]; present {
			t.Errorf("health_checks_disabled leaked to a non-capable caller: %v", f)
		}
		return
	}
	t.Fatal("open-wide missing from the public list")
}
