// Tool inventory for the joe-links MCP server. Every tool is a thin adapter
// over the shared store layer: authorization rules are the REST API's,
// enforced by the same store calls — never reimplemented here.
//
// Governing: ADR-0018, SPEC-0018 REQ "Tool Inventory", REQ "Authorization Parity with the REST API"
package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/llm"
	"github.com/joestump/joe-links/internal/store"
)

// Error codes shared with the REST API vocabulary (one casing).
// Governing: SPEC-0018 REQ "Structured Tool Errors"
const (
	codeUnauthorized   = "unauthorized"
	codeForbidden      = "forbidden"
	codeNotFound       = "not_found"
	codeValidation     = "validation_failed"
	codeDuplicateSlug  = "duplicate_slug"
	codeUnknownUser    = "unknown_user"
	codeDuplicateShare = "duplicate_share"
	codeDuplicateOwner = "duplicate_owner"
	codeLLMError       = "llm_error"
	codeInternal       = "internal_error"
)

// ownerPayload mirrors the REST OwnerResponse shape.
type ownerPayload struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	IsPrimary bool   `json:"is_primary"`
}

// healthPayload mirrors the REST HealthResponse shape byte-for-byte so the
// two surfaces can never disagree on the health object. Status is derived
// from the link_health table in the shared store layer, subject to the
// surfacing rule: opted-out, archived, and expired links report "unchecked"
// with null details even when a frozen health row exists. Present only for
// callers holding capabilities on the link.
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP", REQ "Destination Health Checking"
type healthPayload struct {
	Status        string     `json:"status" jsonschema:"unchecked, ok, broken, or skipped"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	LastStatus    *int       `json:"last_status"`
}

// healthFor derives the capability-gated health object for a link. Returns
// (nil, nil, nil) when the caller holds no capabilities — such callers never
// receive the health object or the opt-out flag, matching REST.
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP" scenario
// "Non-Capable Caller Gets No Health Data"
func healthFor(ctx context.Context, deps Deps, link *store.Link, includeHealth bool) (*healthPayload, *bool, error) {
	if !includeHealth {
		return nil, nil, nil
	}
	hRow, err := deps.LinkStore.GetHealth(ctx, link.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("load link health: %w", err)
	}
	view := store.DeriveHealth(link, hRow, time.Now().UTC())
	disabled := link.HealthChecksDisabled
	return &healthPayload{
		Status:        view.Status,
		LastCheckedAt: view.LastCheckedAt,
		LastStatus:    view.LastStatus,
	}, &disabled, nil
}

// linkPayload is the structured result for tools returning a full link.
// ShortURL is always populated so agents can hand humans a working URL.
//
// Serialization convention (recorded per the PR #290 debt ledger): MCP
// payloads use omitempty for the nullable lifecycle timestamps — an ABSENT
// expires_at/archived_at means null — where REST serializes explicit nulls.
// The REQ's "same lifecycle fields" holds semantically; absent-means-null is
// the pinned MCP wire convention (internal/mcp/lifecycle_test.go, PR #283).
// Governing: SPEC-0018 REQ "Tool Inventory" — short URL in create/get/update results
type linkPayload struct {
	ID             string         `json:"id"`
	Slug           string         `json:"slug"`
	ShortURL       string         `json:"short_url" jsonschema:"canonical short URL for this link"`
	URL            string         `json:"url"`
	Title          string         `json:"title,omitempty"`
	Description    string         `json:"description,omitempty"`
	Visibility     string         `json:"visibility"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty" jsonschema:"RFC 3339 expiration timestamp; absent means the link never expires"`     // Governing: SPEC-0020 REQ "Link Expiration"
	ArchivedAt     *time.Time     `json:"archived_at,omitempty" jsonschema:"RFC 3339 archive timestamp; absent means the link is not archived"`     // Governing: SPEC-0020 REQ "Archive State"
	LifecycleState string         `json:"lifecycle_state" jsonschema:"derived lifecycle state: active, expired, or archived"`                       // Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
	Health         *healthPayload `json:"health,omitempty" jsonschema:"destination health; present only for callers with capabilities on the link"` // Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
	// HealthChecksDisabled is the per-link checker opt-out, present (like the
	// health object) only for callers holding capabilities on the link.
	// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
	HealthChecksDisabled *bool          `json:"health_checks_disabled,omitempty" jsonschema:"per-link health-check opt-out; present only for callers with capabilities on the link"`
	Tags                 []string       `json:"tags,omitempty"`
	Owners               []ownerPayload `json:"owners,omitempty"`
	SharedWith           []string       `json:"shared_with,omitempty" jsonschema:"emails with share access; only populated for owners/admins"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}

// listItemPayload is the compact per-row shape for list_links. It carries the
// same lifecycle and (capability-gated) health fields as get_link.
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
type listItemPayload struct {
	ID                   string         `json:"id"`
	Slug                 string         `json:"slug"`
	ShortURL             string         `json:"short_url"`
	URL                  string         `json:"url"`
	Title                string         `json:"title,omitempty"`
	Description          string         `json:"description,omitempty"`
	Visibility           string         `json:"visibility"`
	ExpiresAt            *time.Time     `json:"expires_at,omitempty"`
	ArchivedAt           *time.Time     `json:"archived_at,omitempty"`
	LifecycleState       string         `json:"lifecycle_state"`
	Health               *healthPayload `json:"health,omitempty"`
	HealthChecksDisabled *bool          `json:"health_checks_disabled,omitempty"`
}

// registerTools registers the SPEC-0018 v1 tool inventory — exactly these
// tools, no admin capabilities. suggest_link_metadata is registered only when
// an LLM provider is configured.
// Governing: SPEC-0018 REQ "Tool Inventory", REQ "Conditional Suggestion Tool"
func registerTools(s *sdk.Server, deps Deps) {
	addTool(s, &sdk.Tool{
		Name:        "create_link",
		Description: "Create a go-link. Defaults to private visibility; pass share_with emails to create a secure link shared with those users in one atomic call. Optionally set a future expires_at (RFC 3339). Returns the working short URL.",
	}, createLinkTool(deps))

	addTool(s, &sdk.Tool{
		Name:        "get_link",
		Description: "Fetch one link by slug or id, including visibility, tags, owners, and (for owners) share grants.",
	}, getLinkTool(deps))

	addTool(s, &sdk.Tool{
		Name:        "list_links",
		Description: "List links visible to you. filter: mine (default), shared (shared with you), or public (browsable by everyone). Optional q search and tag filter apply to mine/public.",
	}, listLinksTool(deps))

	addTool(s, &sdk.Tool{
		Name:        "update_link",
		Description: "Update url, title, description, tags, visibility, expiration, archive state, or the health-check opt-out on a link you own. Omitted fields are left unchanged; slug is immutable. Pass expires_at as null or an empty string to clear the expiration. Pass archived true/false to archive or unarchive (the slug stays reserved and stats are kept either way). Pass health_checks_disabled true/false to opt the destination out of (or back into) health checks.",
	}, updateLinkTool(deps))

	addTool(s, &sdk.Tool{
		Name:        "delete_link",
		Description: "Permanently delete a link you own.",
	}, deleteLinkTool(deps))

	addTool(s, &sdk.Tool{
		Name:        "share_link",
		Description: "Grant a user access to a secure link by email. The user must already have a joe-links account.",
	}, shareLinkTool(deps))

	addTool(s, &sdk.Tool{
		Name:        "unshare_link",
		Description: "Revoke a user's share access to a link by email.",
	}, unshareLinkTool(deps))

	addTool(s, &sdk.Tool{
		Name:        "add_co_owner",
		Description: "Add a co-owner to a link you own, by email. Co-owners can edit and delete the link.",
	}, addCoOwnerTool(deps))

	addTool(s, &sdk.Tool{
		Name:        "get_link_stats",
		Description: "Click totals (all-time, 7d, 30d) and recent clicks for a link you own or that is shared with you.",
	}, getLinkStatsTool(deps))

	// Governing: SPEC-0018 REQ "Conditional Suggestion Tool"
	if deps.Suggester != nil {
		addTool(s, &sdk.Tool{
			Name:        "suggest_link_metadata",
			Description: "Ask the configured LLM to suggest a slug, title, description, and tags for a destination URL.",
		}, suggestTool(deps))
	}

	addTool(s, &sdk.Tool{
		Name:        "list_keywords",
		Description: "List keyword templates configured on this server (used for keyword host routing like jira/ABC-123).",
	}, listKeywordsTool(deps))
}

// --- shared helpers -------------------------------------------------------

// requireUser returns the authenticated user from the request context. The
// bearer middleware guarantees presence; the nil check is defense in depth.
// Governing: SPEC-0018 REQ "Bearer Token Authentication"
func requireUser(ctx context.Context) (*store.User, *sdk.CallToolResult) {
	if u := auth.UserFromContext(ctx); u != nil {
		return u, nil
	}
	return nil, errorResult(codeUnauthorized, "no authenticated user in request context")
}

// resolveLink looks a link up by slug first, then by id.
func resolveLink(ctx context.Context, deps Deps, ref string) (*store.Link, *sdk.CallToolResult) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errorResult(codeValidation, "link reference (slug or id) is required")
	}
	link, err := deps.LinkStore.GetBySlug(ctx, ref)
	if err == nil {
		return link, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		slog.Error("mcp: resolve link by slug", "ref", ref, "error", err)
		return nil, errorResult(codeInternal, "failed to look up link")
	}
	link, err = deps.LinkStore.GetByID(ctx, ref)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound) {
			return nil, errorResult(codeNotFound, fmt.Sprintf("no link with slug or id %q", ref))
		}
		slog.Error("mcp: resolve link by id", "ref", ref, "error", err)
		return nil, errorResult(codeInternal, "failed to look up link")
	}
	return link, nil
}

// isOwnerOrAdmin mirrors the REST mutation rule: link owners (incl. co-owners)
// and admins.
// Governing: SPEC-0018 REQ "Authorization Parity with the REST API"
func isOwnerOrAdmin(deps Deps, user *store.User, linkID string) (bool, error) {
	return store.IsOwnerOrAdmin(deps.OwnershipStore, linkID, user.ID, user.Role)
}

// linkCaps resolves the caller's capability set via the shared store helper so
// MCP can never drift from the REST/web matrix.
// Governing: SPEC-0018 REQ "Authorization Parity with the REST API"
// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
func linkCaps(ctx context.Context, deps Deps, user *store.User, linkID string) (store.LinkCaps, error) {
	return store.LinkCapsFor(ctx, deps.OwnershipStore, deps.LinkStore, linkID, user)
}

// buildLinkPayload assembles the full link result, including owners and — for
// owners/admins — the share list resolved to emails. includeHealth must be
// true only for callers holding capabilities on the link, matching the REST
// gating exactly.
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
func buildLinkPayload(ctx context.Context, deps Deps, link *store.Link, includeShares, includeHealth bool) (*linkPayload, error) {
	p := &linkPayload{
		ID:          link.ID,
		Slug:        link.Slug,
		ShortURL:    shortURL(ctx, link.Slug),
		URL:         link.URL,
		Title:       link.Title,
		Description: link.Description,
		Visibility:  link.Visibility,
		ExpiresAt:   link.ExpiresAt,  // Governing: SPEC-0020 REQ "Link Expiration"
		ArchivedAt:  link.ArchivedAt, // Governing: SPEC-0020 REQ "Archive State"
		// Derived at read time — archived wins when both apply (ADR-0020).
		LifecycleState: link.LifecycleState(time.Now().UTC()),
		CreatedAt:      link.CreatedAt,
		UpdatedAt:      link.UpdatedAt,
	}
	health, disabled, err := healthFor(ctx, deps, link, includeHealth)
	if err != nil {
		return nil, err
	}
	p.Health = health
	p.HealthChecksDisabled = disabled

	tags, err := deps.LinkStore.ListTags(ctx, link.ID)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	for _, t := range tags {
		p.Tags = append(p.Tags, t.Name)
	}

	owners, err := deps.OwnershipStore.ListOwnerUsers(link.ID)
	if err != nil {
		return nil, fmt.Errorf("list owners: %w", err)
	}
	for _, o := range owners {
		p.Owners = append(p.Owners, ownerPayload{ID: o.ID, Email: o.Email, IsPrimary: o.IsPrimary})
	}

	if includeShares {
		shares, err := deps.LinkStore.ListShares(ctx, link.ID)
		if err != nil {
			return nil, fmt.Errorf("list shares: %w", err)
		}
		for _, sh := range shares {
			u, err := deps.UserStore.GetByID(ctx, sh.UserID)
			if err != nil {
				continue // dangling share row; skip rather than fail the read
			}
			p.SharedWith = append(p.SharedWith, u.Email)
		}
		sort.Strings(p.SharedWith)
	}
	return p, nil
}

// resolveEmails maps share_with emails to user IDs. All emails must resolve;
// unknown ones fail the whole call, named.
// Governing: SPEC-0018 REQ "Agent-Oriented Creation Defaults"
func resolveEmails(ctx context.Context, deps Deps, emails []string) ([]string, *sdk.CallToolResult) {
	ids := make([]string, 0, len(emails))
	var unknown []string
	for _, e := range emails {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		u, err := deps.UserStore.GetByEmail(ctx, e)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				unknown = append(unknown, e)
				continue
			}
			slog.Error("mcp: resolve email", "error", err)
			return nil, errorResult(codeInternal, "failed to resolve user email")
		}
		ids = append(ids, u.ID)
	}
	if len(unknown) > 0 {
		return nil, errorResult(codeUnknownUser,
			fmt.Sprintf("no joe-links account for: %s — users must sign in once before links can be shared with them", strings.Join(unknown, ", ")))
	}
	return ids, nil
}

func internalError(action string, err error) *sdk.CallToolResult {
	slog.Error("mcp: "+action, "error", err)
	return errorResult(codeInternal, action+" failed")
}

// parseExpiresAtInput parses an RFC 3339 expires_at tool argument. An empty
// string means "no expiration" (create) / "clear it" (update), returning nil.
// Governing: SPEC-0020 REQ "Link Expiration"
func parseExpiresAtInput(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("expires_at must be an RFC 3339 timestamp (e.g. 2030-01-02T15:04:05Z): %w", err)
	}
	return &t, nil
}

// --- create_link ----------------------------------------------------------

type createLinkIn struct {
	Slug        string   `json:"slug" jsonschema:"short slug, [a-z0-9][a-z0-9-]*[a-z0-9]"`
	URL         string   `json:"url" jsonschema:"destination URL; may contain $variable placeholders"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Visibility  string   `json:"visibility,omitempty" jsonschema:"public, private, or secure; defaults to private (secure when share_with is set)"`
	ExpiresAt   string   `json:"expires_at,omitempty" jsonschema:"optional RFC 3339 expiration timestamp (must be in the future); omit for a link that never expires"` // Governing: SPEC-0020 REQ "Link Expiration"
	// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP" — same
	// health_checks_disabled input as POST /api/v1/links
	HealthChecksDisabled bool     `json:"health_checks_disabled,omitempty" jsonschema:"true opts this link out of destination health checks"`
	ShareWith            []string `json:"share_with,omitempty" jsonschema:"emails of existing users to grant access; implies secure visibility unless overridden"`
}

func createLinkTool(deps Deps) sdk.ToolHandlerFor[createLinkIn, *linkPayload] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in createLinkIn) (*sdk.CallToolResult, *linkPayload, error) {
		user, denied := requireUser(ctx)
		if denied != nil {
			return denied, nil, nil
		}

		if strings.TrimSpace(in.URL) == "" {
			return errorResult(codeValidation, "url is required"), nil, nil
		}
		if err := store.ValidateSlugFormat(in.Slug); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}
		// Scheme allowlist: only http(s) destinations may be stored (issue #265).
		if err := store.ValidateLinkURL(in.URL); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}
		if err := store.ValidateURLVariables(in.URL); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}
		if err := store.ValidateLinkText(in.Title, in.Description); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}
		// Tag intake validation: safe charset, bounded length and count, shared
		// with the REST API and web forms via the store validators (issues #251, #265).
		if err := store.ValidateTagNames(in.Tags); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}

		// Agent-surface defaults: private unless sharing, which implies secure.
		// Explicit visibility always wins. Deliberate divergence from the REST
		// default of public — see SPEC-0018.
		// Governing: SPEC-0018 REQ "Agent-Oriented Creation Defaults"
		visibility := in.Visibility
		if visibility == "" {
			if len(in.ShareWith) > 0 {
				visibility = "secure"
			} else {
				visibility = "private"
			}
		}
		if err := store.ValidateVisibility(visibility); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}

		// Optional expiration: parsed and validated exactly like the REST API
		// (a past value is rejected before any row is written).
		// Governing: SPEC-0020 REQ "Link Expiration" scenarios "Link Created
		// with Expiration", "Past Expiration Rejected"
		expiresAt, err := parseExpiresAtInput(in.ExpiresAt)
		if err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}
		if err := store.ValidateExpiresAt(expiresAt, nil, time.Now().UTC()); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}

		shareIDs, denied := resolveEmails(ctx, deps, in.ShareWith)
		if denied != nil {
			return denied, nil, nil
		}

		// Atomic: link + owner + tags + shares in one transaction.
		// Governing: SPEC-0018 REQ "Database Operation Standards"
		link, err := deps.LinkStore.CreateFull(ctx, in.Slug, in.URL, user.ID, in.Title, in.Description, visibility, expiresAt, in.Tags, shareIDs, user.ID)
		if err != nil {
			if errors.Is(err, store.ErrSlugTaken) {
				return errorResult(codeDuplicateSlug, fmt.Sprintf("slug %q already exists", in.Slug)), nil, nil
			}
			// The store re-validates expires_at with a later clock than the
			// check above; a value that expires inside the request window must
			// still surface as a validation error, not an internal one.
			// Governing: SPEC-0020 REQ "Link Expiration" scenario "Past Expiration Rejected"
			if errors.Is(err, store.ErrExpiresAtInPast) {
				return errorResult(codeValidation, err.Error()), nil, nil
			}
			return internalError("create link", err), nil, nil
		}

		// Optional health-check opt-out on create: the creator is the primary
		// owner, so CanEdit is held by construction — matching REST.
		// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
		if in.HealthChecksDisabled {
			link, err = deps.LinkStore.SetHealthChecksDisabled(ctx, link.ID, true)
			if err != nil {
				return internalError("set health-check opt-out", err), nil, nil
			}
		}

		// The creator is the primary owner: shares and health both included.
		p, err := buildLinkPayload(ctx, deps, link, true, true)
		if err != nil {
			return internalError("build link result", err), nil, nil
		}
		return nil, p, nil
	}
}

// --- get_link ---------------------------------------------------------------

type linkRefIn struct {
	Link string `json:"link" jsonschema:"link slug or id"`
}

func getLinkTool(deps Deps) sdk.ToolHandlerFor[linkRefIn, *linkPayload] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in linkRefIn) (*sdk.CallToolResult, *linkPayload, error) {
		user, denied := requireUser(ctx)
		if denied != nil {
			return denied, nil, nil
		}
		link, errRes := resolveLink(ctx, deps, in.Link)
		if errRes != nil {
			return errRes, nil, nil
		}

		caps, err := linkCaps(ctx, deps, user, link.ID)
		if err != nil {
			return internalError("authorize read", err), nil, nil
		}
		if !caps.CanView {
			return errorResult(codeForbidden, "you do not have access to this link"), nil, nil
		}

		// Share roster is visible to share managers only, never to recipients.
		// Health goes to anyone past the CanView gate — every such caller
		// holds capabilities on the link (owners, co-owners, admins, share
		// recipients), matching REST.
		// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
		p, err := buildLinkPayload(ctx, deps, link, caps.CanManageShares, caps.CanView)
		if err != nil {
			return internalError("build link result", err), nil, nil
		}
		return nil, p, nil
	}
}

// --- list_links ---------------------------------------------------------------

type listLinksIn struct {
	Q      string `json:"q,omitempty" jsonschema:"search slugs, URLs, titles, and descriptions"`
	Tag    string `json:"tag,omitempty" jsonschema:"filter by tag slug (mine filter only)"`
	Filter string `json:"filter,omitempty" jsonschema:"mine (default), shared, or public"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results, default 50, cap 200"`
}

type listLinksOut struct {
	Links []listItemPayload `json:"links"`
	Count int               `json:"count"`
}

func listLinksTool(deps Deps) sdk.ToolHandlerFor[listLinksIn, *listLinksOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in listLinksIn) (*sdk.CallToolResult, *listLinksOut, error) {
		user, denied := requireUser(ctx)
		if denied != nil {
			return denied, nil, nil
		}

		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}

		out := &listLinksOut{Links: []listItemPayload{}}
		now := time.Now().UTC()
		// includeHealth gates the health object per row: true only when the
		// caller holds capabilities on the link, matching REST and get_link.
		// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
		var appendErr error
		appendLink := func(l *store.Link, includeHealth bool) {
			if appendErr != nil {
				return
			}
			item := listItemPayload{
				ID: l.ID, Slug: l.Slug, ShortURL: shortURL(ctx, l.Slug),
				URL: l.URL, Title: l.Title, Description: l.Description, Visibility: l.Visibility,
				ExpiresAt: l.ExpiresAt, ArchivedAt: l.ArchivedAt, LifecycleState: l.LifecycleState(now),
			}
			health, disabled, err := healthFor(ctx, deps, l, includeHealth)
			if err != nil {
				appendErr = err
				return
			}
			item.Health = health
			item.HealthChecksDisabled = disabled
			out.Links = append(out.Links, item)
		}

		switch in.Filter {
		case "", "mine":
			var links []*store.Link
			var err error
			switch {
			case in.Q != "":
				links, err = deps.LinkStore.SearchByOwner(ctx, user.ID, in.Q)
			case in.Tag != "":
				links, err = deps.LinkStore.ListByOwnerAndTag(ctx, user.ID, in.Tag)
			default:
				links, err = deps.LinkStore.ListByOwner(ctx, user.ID)
			}
			if err != nil {
				return internalError("list links", err), nil, nil
			}
			for _, l := range links {
				if len(out.Links) >= limit {
					break
				}
				// Every "mine" row is owned by the caller — capabilities held.
				appendLink(l, true)
			}
		case "shared":
			links, err := deps.LinkStore.ListSharedWithUser(ctx, user.ID)
			if err != nil {
				return internalError("list shared links", err), nil, nil
			}
			for _, l := range links {
				if len(out.Links) >= limit {
					break
				}
				// Share recipients hold capabilities (CanView/CanStats) on
				// every shared row, so health is included.
				appendLink(l, true)
			}
		case "public":
			// Governing: SPEC-0018 REQ "Authorization Parity with the REST API"
			// — public listing never exposes other users' private/secure links.
			links, _, err := deps.LinkStore.ListPublic(ctx, user.ID, in.Q, 1, limit)
			if err != nil {
				return internalError("list public links", err), nil, nil
			}
			// The public listing mixes rows the caller may or may not hold
			// capabilities on; health is gated per row.
			ids := make([]string, len(links))
			for i, l := range links {
				ids[i] = l.ID
			}
			rowCaps, err := store.LinkCapsForAll(ctx, deps.OwnershipStore, deps.LinkStore, ids, user)
			if err != nil {
				return internalError("authorize health fields", err), nil, nil
			}
			for _, l := range links {
				appendLink(&l.Link, rowCaps[l.ID].CanView)
			}
		default:
			return errorResult(codeValidation, `filter must be "mine", "shared", or "public"`), nil, nil
		}
		if appendErr != nil {
			return internalError("build link rows", appendErr), nil, nil
		}

		out.Count = len(out.Links)
		return nil, out, nil
	}
}

// --- update_link ---------------------------------------------------------------

type updateLinkIn struct {
	Link        string    `json:"link" jsonschema:"link slug or id"`
	URL         *string   `json:"url,omitempty"`
	Title       *string   `json:"title,omitempty"`
	Description *string   `json:"description,omitempty"`
	Tags        *[]string `json:"tags,omitempty" jsonschema:"replaces the full tag set; [] clears all tags"`
	Visibility  *string   `json:"visibility,omitempty" jsonschema:"public, private, or secure"`
	ExpiresAt   *string   `json:"expires_at,omitempty" jsonschema:"RFC 3339 expiration timestamp; null or an empty string clears the expiration; omitted leaves it unchanged"` // Governing: SPEC-0020 REQ "Link Expiration"
	Archived    *bool     `json:"archived,omitempty" jsonschema:"true archives the link (resolution stops, slug stays reserved, stats kept); false unarchives"`                // Governing: SPEC-0020 REQ "Archive State"
	// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP" — same
	// health_checks_disabled input as PUT /api/v1/links/{id}
	HealthChecksDisabled *bool `json:"health_checks_disabled,omitempty" jsonschema:"true opts this link out of destination health checks; false opts back in; omitted leaves it unchanged"`
}

// rawArgIsNull reports whether the raw tool arguments contain key with an
// explicit JSON null — which encoding/json cannot distinguish from an omitted
// key once decoded into a pointer field.
func rawArgIsNull(req *sdk.CallToolRequest, key string) bool {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return false
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return false
	}
	raw, ok := args[key]
	return ok && string(bytes.TrimSpace(raw)) == "null"
}

func updateLinkTool(deps Deps) sdk.ToolHandlerFor[updateLinkIn, *linkPayload] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in updateLinkIn) (*sdk.CallToolResult, *linkPayload, error) {
		user, denied := requireUser(ctx)
		if denied != nil {
			return denied, nil, nil
		}
		link, errRes := resolveLink(ctx, deps, in.Link)
		if errRes != nil {
			return errRes, nil, nil
		}
		ok, err := isOwnerOrAdmin(deps, user, link.ID)
		if err != nil {
			return internalError("authorize update", err), nil, nil
		}
		if !ok {
			return errorResult(codeForbidden, "only owners and admins may update a link"), nil, nil
		}

		// Overlay provided fields on current values; omitted fields unchanged.
		url, title, description, visibility := link.URL, link.Title, link.Description, link.Visibility
		if in.URL != nil {
			url = *in.URL
		}
		if in.Title != nil {
			title = *in.Title
		}
		if in.Description != nil {
			description = *in.Description
		}
		if in.Visibility != nil {
			visibility = *in.Visibility
		}
		if strings.TrimSpace(url) == "" {
			return errorResult(codeValidation, "url cannot be empty"), nil, nil
		}
		// Scheme allowlist: only http(s) destinations may be stored (issue #265).
		if err := store.ValidateLinkURL(url); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}
		if err := store.ValidateURLVariables(url); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}
		if err := store.ValidateLinkText(title, description); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}
		if err := store.ValidateVisibility(visibility); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}

		// Expiration overlay: omitted leaves the stored value unchanged; an
		// empty string OR an explicit JSON null clears it; a NEW past value is
		// rejected — identical validation to PUT /api/v1/links/{id}. Capability
		// gating comes from the owner/admin check above: share recipients never
		// reach this write.
		// Governing: SPEC-0020 REQ "Link Expiration" scenarios "Past Expiration
		// Rejected", "Expired Link Stays Editable", "Expiration Cleared on
		// Edit", "Share Recipient Cannot Set Expiry"
		expiresAt := link.ExpiresAt
		switch {
		case in.ExpiresAt != nil:
			parsed, err := parseExpiresAtInput(*in.ExpiresAt)
			if err != nil {
				return errorResult(codeValidation, err.Error()), nil, nil
			}
			expiresAt = parsed
		case rawArgIsNull(req, "expires_at"):
			// REST parity: PUT /links/{id} treats an explicit JSON null as
			// "clear the expiration" (api.OptionalTime), but a *string field
			// decodes null to nil — indistinguishable from omitted — which
			// silently left the value unchanged (the PR #283 asymmetry). Probe
			// the raw arguments so the two surfaces agree.
			// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP" —
			// update_link accepts the same lifecycle inputs as the REST API
			expiresAt = nil
		}
		if err := store.ValidateExpiresAt(expiresAt, link.ExpiresAt, time.Now().UTC()); err != nil {
			return errorResult(codeValidation, err.Error()), nil, nil
		}

		// Validate the raw tag list BEFORE the row update so a hostile tag
		// name cannot leave the link half-updated, and before deduping so the
		// count cap and empty-entry rejection see the same raw input as
		// create_link and the REST API (issues #251, #265). The web form
		// differs by construction: parseTagNames strips empty segments before
		// validation, so empty entries cannot reach the validator there.
		// Then dedupe case-insensitively so duplicate spellings cannot roll
		// the tag write back (tags upsert by derived slug).
		var tags []string
		if in.Tags != nil {
			if err := store.ValidateTagNames(*in.Tags); err != nil {
				return errorResult(codeValidation, err.Error()), nil, nil
			}
			seen := map[string]bool{}
			for _, tag := range *in.Tags {
				k := strings.ToLower(strings.TrimSpace(tag))
				if seen[k] {
					continue
				}
				seen[k] = true
				tags = append(tags, tag)
			}
		}

		updated, err := deps.LinkStore.Update(ctx, link.ID, url, title, description, visibility, expiresAt)
		if err != nil {
			// The store re-validates expires_at with a later clock than the
			// check above; a value that expires inside the request window must
			// still surface as a validation error, not an internal one.
			// Governing: SPEC-0020 REQ "Link Expiration" scenario "Past Expiration Rejected"
			if errors.Is(err, store.ErrExpiresAtInPast) {
				return errorResult(codeValidation, err.Error()), nil, nil
			}
			return internalError("update link", err), nil, nil
		}

		// Archive toggle: an ordinary edit under the same CanEdit gate,
		// identical to PUT /api/v1/links/{id}'s archived field. true stamps
		// archived_at (if not already set), false clears it, omitted leaves
		// the archive state unchanged.
		// Governing: SPEC-0020 REQ "Archive State", REQ "Lifecycle State in API and MCP"
		if in.Archived != nil {
			updated, err = deps.LinkStore.SetArchived(ctx, updated.ID, *in.Archived)
			if err != nil {
				return internalError("update archive state", err), nil, nil
			}
		}

		// Health-check opt-out toggle, identical to PUT /api/v1/links/{id}'s
		// health_checks_disabled field: an ordinary edit under the same
		// CanEdit gate; omitted leaves the stored flag unchanged.
		// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
		if in.HealthChecksDisabled != nil {
			updated, err = deps.LinkStore.SetHealthChecksDisabled(ctx, updated.ID, *in.HealthChecksDisabled)
			if err != nil {
				return internalError("update health-check opt-out", err), nil, nil
			}
		}

		if in.Tags != nil {
			if err := deps.LinkStore.SetTags(ctx, updated.ID, tags); err != nil {
				// Mirror REST's TAG_WRITE_FAILED semantics: the link row is
				// already committed, tags are not; the call is retryable.
				return internalError("update tags (link updated but tags could not be saved; retry)", err), nil, nil
			}
		}

		// Only owners/co-owners/admins reach a successful update: shares and
		// health both included.
		p, err := buildLinkPayload(ctx, deps, updated, true, true)
		if err != nil {
			return internalError("build link result", err), nil, nil
		}
		return nil, p, nil
	}
}

// --- delete_link ---------------------------------------------------------------

type deleteLinkOut struct {
	Deleted bool   `json:"deleted"`
	Slug    string `json:"slug"`
}

func deleteLinkTool(deps Deps) sdk.ToolHandlerFor[linkRefIn, *deleteLinkOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in linkRefIn) (*sdk.CallToolResult, *deleteLinkOut, error) {
		user, denied := requireUser(ctx)
		if denied != nil {
			return denied, nil, nil
		}
		link, errRes := resolveLink(ctx, deps, in.Link)
		if errRes != nil {
			return errRes, nil, nil
		}
		ok, err := isOwnerOrAdmin(deps, user, link.ID)
		if err != nil {
			return internalError("authorize delete", err), nil, nil
		}
		if !ok {
			return errorResult(codeForbidden, "only owners and admins may delete a link"), nil, nil
		}
		if err := deps.LinkStore.Delete(ctx, link.ID); err != nil {
			return internalError("delete link", err), nil, nil
		}
		return nil, &deleteLinkOut{Deleted: true, Slug: link.Slug}, nil
	}
}

// --- share_link / unshare_link / add_co_owner ---------------------------------

type shareIn struct {
	Link  string `json:"link" jsonschema:"link slug or id"`
	Email string `json:"email" jsonschema:"email of an existing joe-links user"`
}

type shareOut struct {
	Link       string   `json:"link"`
	SharedWith []string `json:"shared_with"`
}

// requireShareTarget resolves the link + target user and checks owner/admin.
func requireShareTarget(ctx context.Context, deps Deps, in shareIn) (*store.Link, *store.User, *sdk.CallToolResult) {
	user, denied := requireUser(ctx)
	if denied != nil {
		return nil, nil, denied
	}
	link, errRes := resolveLink(ctx, deps, in.Link)
	if errRes != nil {
		return nil, nil, errRes
	}
	ok, err := isOwnerOrAdmin(deps, user, link.ID)
	if err != nil {
		return nil, nil, internalError("authorize share management", err)
	}
	if !ok {
		return nil, nil, errorResult(codeForbidden, "only owners and admins may manage shares")
	}
	target, err := deps.UserStore.GetByEmail(ctx, strings.TrimSpace(in.Email))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, errorResult(codeUnknownUser,
				fmt.Sprintf("no joe-links account for %s — users must sign in once before links can be shared with them", in.Email))
		}
		return nil, nil, internalError("resolve user email", err)
	}
	return link, target, nil
}

func sharesResult(ctx context.Context, deps Deps, link *store.Link) (*sdk.CallToolResult, *shareOut, error) {
	// includeHealth=true: every caller reaching here passed the owner/admin
	// gate and so holds capabilities on the link — the health fields are
	// built consistently with the capability gating even though shareOut
	// serializes only the roster (PR #290 debt ledger, item 4).
	// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
	p, err := buildLinkPayload(ctx, deps, link, true, true)
	if err != nil {
		return internalError("list shares", err), nil, nil
	}
	shared := p.SharedWith
	if shared == nil {
		shared = []string{}
	}
	return nil, &shareOut{Link: link.Slug, SharedWith: shared}, nil
}

func shareLinkTool(deps Deps) sdk.ToolHandlerFor[shareIn, *shareOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in shareIn) (*sdk.CallToolResult, *shareOut, error) {
		user := auth.UserFromContext(ctx)
		link, target, errRes := requireShareTarget(ctx, deps, in)
		if errRes != nil {
			return errRes, nil, nil
		}
		has, err := deps.LinkStore.HasShare(ctx, link.ID, target.ID)
		if err != nil {
			return internalError("check existing share", err), nil, nil
		}
		if has {
			return errorResult(codeDuplicateShare, fmt.Sprintf("%s already has access to %s", in.Email, link.Slug)), nil, nil
		}
		if err := deps.LinkStore.AddShare(ctx, link.ID, target.ID, user.ID); err != nil {
			return internalError("add share", err), nil, nil
		}
		return sharesResult(ctx, deps, link)
	}
}

func unshareLinkTool(deps Deps) sdk.ToolHandlerFor[shareIn, *shareOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in shareIn) (*sdk.CallToolResult, *shareOut, error) {
		link, target, errRes := requireShareTarget(ctx, deps, in)
		if errRes != nil {
			return errRes, nil, nil
		}
		if err := deps.LinkStore.RemoveShare(ctx, link.ID, target.ID); err != nil {
			return internalError("remove share", err), nil, nil
		}
		return sharesResult(ctx, deps, link)
	}
}

type coOwnerOut struct {
	Link   string         `json:"link"`
	Owners []ownerPayload `json:"owners"`
}

func addCoOwnerTool(deps Deps) sdk.ToolHandlerFor[shareIn, *coOwnerOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in shareIn) (*sdk.CallToolResult, *coOwnerOut, error) {
		link, target, errRes := requireShareTarget(ctx, deps, in)
		if errRes != nil {
			return errRes, nil, nil
		}
		if err := deps.LinkStore.AddOwner(ctx, link.ID, target.ID); err != nil {
			if errors.Is(err, store.ErrDuplicateOwner) || errors.Is(err, store.ErrAlreadyOwner) {
				return errorResult(codeDuplicateOwner, fmt.Sprintf("%s already owns %s", in.Email, link.Slug)), nil, nil
			}
			return internalError("add co-owner", err), nil, nil
		}
		// includeHealth=true: the caller passed the owner/admin gate, so the
		// health fields are built consistently with the capability gating
		// (PR #290 debt ledger, item 4).
		p, err := buildLinkPayload(ctx, deps, link, false, true)
		if err != nil {
			return internalError("list owners", err), nil, nil
		}
		return nil, &coOwnerOut{Link: link.Slug, Owners: p.Owners}, nil
	}
}

// --- get_link_stats -------------------------------------------------------------

type statsIn struct {
	Link  string `json:"link" jsonschema:"link slug or id"`
	Limit int    `json:"limit,omitempty" jsonschema:"max recent clicks, default 20, cap 100"`
}

type recentClickPayload struct {
	ClickedAt time.Time `json:"clicked_at"`
	Referrer  string    `json:"referrer,omitempty"`
	User      string    `json:"user,omitempty" jsonschema:"display name of the authenticated clicker, empty for anonymous"`
}

type statsOut struct {
	Link    string               `json:"link"`
	Total   int64                `json:"total"`
	Last7d  int64                `json:"last_7d"`
	Last30d int64                `json:"last_30d"`
	Recent  []recentClickPayload `json:"recent"`
}

func getLinkStatsTool(deps Deps) sdk.ToolHandlerFor[statsIn, *statsOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in statsIn) (*sdk.CallToolResult, *statsOut, error) {
		user, denied := requireUser(ctx)
		if denied != nil {
			return denied, nil, nil
		}
		link, errRes := resolveLink(ctx, deps, in.Link)
		if errRes != nil {
			return errRes, nil, nil
		}
		// REST stats rule: owners, share recipients, and admins may read stats.
		// Governing: SPEC-0018 REQ "Authorization Parity with the REST API"
		// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
		caps, err := linkCaps(ctx, deps, user, link.ID)
		if err != nil {
			return internalError("authorize stats", err), nil, nil
		}
		if !caps.CanStats {
			return errorResult(codeForbidden, "only owners, share recipients, and admins may view stats"), nil, nil
		}

		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}

		stats, err := deps.ClickStore.GetClickStats(ctx, link.ID)
		if err != nil {
			return internalError("load click stats", err), nil, nil
		}
		recent, err := deps.ClickStore.ListRecentClicks(ctx, link.ID, limit)
		if err != nil {
			return internalError("load recent clicks", err), nil, nil
		}

		out := &statsOut{Link: link.Slug, Total: stats.Total, Last7d: stats.Last7d, Last30d: stats.Last30d, Recent: []recentClickPayload{}}
		for _, c := range recent {
			p := recentClickPayload{ClickedAt: c.ClickedAt, Referrer: c.Referrer}
			// Clicker attribution is manager-only, matching REST: authenticated
			// clickers on a secure link proxy the hidden share roster.
			// Governing: SPEC-0018 REQ "Authorization Parity with the REST API"
			if caps.CanManageShares {
				p.User = c.DisplayName
			}
			out.Recent = append(out.Recent, p)
		}
		return nil, out, nil
	}
}

// --- suggest_link_metadata -------------------------------------------------------

type suggestIn struct {
	URL string `json:"url" jsonschema:"destination URL to suggest metadata for"`
}

type suggestOut struct {
	Slug        string   `json:"slug,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

func suggestTool(deps Deps) sdk.ToolHandlerFor[suggestIn, *suggestOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in suggestIn) (*sdk.CallToolResult, *suggestOut, error) {
		if _, denied := requireUser(ctx); denied != nil {
			return denied, nil, nil
		}
		if strings.TrimSpace(in.URL) == "" {
			return errorResult(codeValidation, "url is required"), nil, nil
		}
		resp, err := deps.Suggester.Suggest(ctx, llm.SuggestRequest{URL: in.URL})
		if err != nil {
			var malformed *llm.MalformedResponseError
			if errors.As(err, &malformed) {
				slog.Error("mcp: LLM suggest malformed response", "error", malformed.Err)
			} else {
				slog.Error("mcp: LLM suggest", "error", err)
			}
			return errorResult(codeLLMError, "LLM provider error"), nil, nil
		}
		// Mirror the REST behavior: blank out invalid slugs, keep the rest.
		// Governing: SPEC-0017 REQ "Default Prompt Template"
		slug := resp.Slug
		if slug != "" && store.ValidateSlugFormat(slug) != nil {
			slug = ""
		}
		return nil, &suggestOut{Slug: slug, Title: resp.Title, Description: resp.Description, Tags: resp.Tags}, nil
	}
}

// --- list_keywords ----------------------------------------------------------------

type keywordsOut struct {
	Keywords []string `json:"keywords"`
}

func listKeywordsTool(deps Deps) sdk.ToolHandlerFor[any, *keywordsOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, _ any) (*sdk.CallToolResult, *keywordsOut, error) {
		if _, denied := requireUser(ctx); denied != nil {
			return denied, nil, nil
		}
		list, err := deps.KeywordStore.List(ctx)
		if err != nil {
			return internalError("list keywords", err), nil, nil
		}
		out := &keywordsOut{Keywords: []string{}}
		for _, k := range list {
			out.Keywords = append(out.Keywords, k.Keyword)
		}
		return nil, out, nil
	}
}
