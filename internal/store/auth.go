// Governing: SPEC-0002 REQ "Authorization Based on Ownership", ADR-0005
// Governing: SPEC-0010 REQ "Link Shares Table", REQ "Secure Link Resolution"
// Governing: SPEC-0016 REQ "Link Stats Dashboard Page"
package store

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// IsOwnerOrAdmin returns true if userID appears in link_owners for linkID, OR role == "admin".
// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
func IsOwnerOrAdmin(ownerStore *OwnershipStore, linkID, userID, role string) (bool, error) {
	if role == "admin" {
		return true, nil
	}
	return ownerStore.IsOwner(linkID, userID)
}

// LinkCaps is the single role→capability matrix for a link, shared by the web
// UI, REST API, and MCP surfaces so they can never disagree on who may do what:
//
//	owner / co-owner / admin: view, stats, edit, delete, manage owners+shares
//	share recipient:          view, stats (read-only)
//	anyone else:              nothing
//
// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
// Governing: SPEC-0016 REQ "Link Stats Dashboard Page"
type LinkCaps struct {
	CanView         bool // link detail page / GET /links/{id} / get_link
	CanStats        bool // stats page / stats+clicks endpoints / get_link_stats
	CanEdit         bool // edit forms / PUT /links/{id} / update_link
	CanDelete       bool // DELETE /links/{id} / delete_link
	CanManageShares bool // owner + share management endpoints/tools
}

// NewLinkCaps computes the capability set from the three role facts.
func NewLinkCaps(isOwner, hasShare, isAdmin bool) LinkCaps {
	manage := isOwner || isAdmin
	return LinkCaps{
		CanView:         manage || hasShare,
		CanStats:        manage || hasShare,
		CanEdit:         manage,
		CanDelete:       manage,
		CanManageShares: manage,
	}
}

// LinkCapsFor resolves the viewer's capabilities on a link using the existing
// ownership and share primitives. A nil user (anonymous) has no capabilities.
// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
// Governing: SPEC-0010 REQ "Link Shares Table"
func LinkCapsFor(ctx context.Context, owns *OwnershipStore, links *LinkStore, linkID string, user *User) (LinkCaps, error) {
	if user == nil {
		return LinkCaps{}, nil
	}
	if user.IsAdmin() {
		return NewLinkCaps(false, false, true), nil
	}
	isOwner, err := owns.IsOwner(linkID, user.ID)
	if err != nil {
		return LinkCaps{}, err
	}
	if isOwner {
		return NewLinkCaps(true, false, false), nil
	}
	hasShare, err := links.HasShare(ctx, linkID, user.ID)
	if err != nil {
		return LinkCaps{}, err
	}
	return NewLinkCaps(false, hasShare, false), nil
}

// LinkCapsForAll resolves the viewer's capabilities for a whole list of links
// in two batched queries (owned set + shared set) instead of two per row —
// list renders re-run on every HTMX keystroke, so per-row queries compound
// fast. Semantics are identical to calling LinkCapsFor per ID.
// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
// Governing: SPEC-0010 REQ "Link Shares Table"
func LinkCapsForAll(ctx context.Context, owns *OwnershipStore, links *LinkStore, linkIDs []string, user *User) (map[string]LinkCaps, error) {
	caps := make(map[string]LinkCaps, len(linkIDs))
	if user == nil || len(linkIDs) == 0 {
		for _, id := range linkIDs {
			caps[id] = LinkCaps{}
		}
		return caps, nil
	}
	if user.IsAdmin() {
		for _, id := range linkIDs {
			caps[id] = NewLinkCaps(false, false, true)
		}
		return caps, nil
	}
	owned, err := owns.ownedSet(ctx, user.ID, linkIDs)
	if err != nil {
		return nil, err
	}
	shared, err := links.sharedSet(ctx, user.ID, linkIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range linkIDs {
		caps[id] = NewLinkCaps(owned[id], shared[id], false)
	}
	return caps, nil
}

// idSet runs a two-column membership query ("which of linkIDs is userID
// attached to in this table?") and returns the matching IDs as a set.
func idSet(ctx context.Context, db *sqlx.DB, table, userID string, linkIDs []string) (map[string]bool, error) {
	query, args, err := sqlx.In(
		`SELECT link_id FROM `+table+` WHERE user_id = ? AND link_id IN (?)`, userID, linkIDs)
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := db.SelectContext(ctx, &ids, db.Rebind(query), args...); err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

// ownedSet returns the subset of linkIDs that userID owns or co-owns.
func (s *OwnershipStore) ownedSet(ctx context.Context, userID string, linkIDs []string) (map[string]bool, error) {
	return idSet(ctx, s.db, "link_owners", userID, linkIDs)
}

// sharedSet returns the subset of linkIDs shared with userID.
func (s *LinkStore) sharedSet(ctx context.Context, userID string, linkIDs []string) (map[string]bool, error) {
	return idSet(ctx, s.db, "link_shares", userID, linkIDs)
}
