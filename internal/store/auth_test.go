// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
package store_test

import (
	"context"
	"testing"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// TestNewLinkCaps_Matrix pins the pure role→capability matrix.
func TestNewLinkCaps_Matrix(t *testing.T) {
	cases := []struct {
		name                       string
		isOwner, hasShare, isAdmin bool
		want                       store.LinkCaps
	}{
		{"owner", true, false, false, store.LinkCaps{CanView: true, CanStats: true, CanEdit: true, CanDelete: true, CanManageShares: true}},
		{"admin", false, false, true, store.LinkCaps{CanView: true, CanStats: true, CanEdit: true, CanDelete: true, CanManageShares: true}},
		{"recipient", false, true, false, store.LinkCaps{CanView: true, CanStats: true}},
		{"owner-and-recipient", true, true, false, store.LinkCaps{CanView: true, CanStats: true, CanEdit: true, CanDelete: true, CanManageShares: true}},
		{"none", false, false, false, store.LinkCaps{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.NewLinkCaps(tc.isOwner, tc.hasShare, tc.isAdmin); got != tc.want {
				t.Errorf("NewLinkCaps(%v, %v, %v) = %+v, want %+v", tc.isOwner, tc.hasShare, tc.isAdmin, got, tc.want)
			}
		})
	}
}

// TestLinkCapsFor_RoleMatrix drives the resolver against a real database for
// every principal kind: primary owner, co-owner, share recipient, admin,
// stranger, and anonymous (nil user).
func TestLinkCapsFor_RoleMatrix(t *testing.T) {
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	ctx := context.Background()

	owner, err := us.Upsert(ctx, "test", "caps-owner", "caps-owner@example.com", "Owner", "user")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	coowner, err := us.Upsert(ctx, "test", "caps-coowner", "caps-coowner@example.com", "CoOwner", "user")
	if err != nil {
		t.Fatalf("seed co-owner: %v", err)
	}
	recipient, err := us.Upsert(ctx, "test", "caps-recipient", "caps-recipient@example.com", "Recipient", "user")
	if err != nil {
		t.Fatalf("seed recipient: %v", err)
	}
	stranger, err := us.Upsert(ctx, "test", "caps-stranger", "caps-stranger@example.com", "Stranger", "user")
	if err != nil {
		t.Fatalf("seed stranger: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "caps-admin", "caps-admin@example.com", "Admin", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	link, err := ls.Create(ctx, "caps-link", "https://internal.example.com/caps", owner.ID, "", "", "secure")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := ls.AddOwner(ctx, link.ID, coowner.ID); err != nil {
		t.Fatalf("seed co-owner grant: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, recipient.ID, owner.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	full := store.LinkCaps{CanView: true, CanStats: true, CanEdit: true, CanDelete: true, CanManageShares: true}
	readOnly := store.LinkCaps{CanView: true, CanStats: true}

	cases := []struct {
		name string
		user *store.User
		want store.LinkCaps
	}{
		{"owner", owner, full},
		{"co-owner", coowner, full},
		{"admin", admin, full},
		{"share-recipient", recipient, readOnly},
		{"stranger", stranger, store.LinkCaps{}},
		{"anonymous", nil, store.LinkCaps{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.LinkCapsFor(ctx, owns, ls, link.ID, tc.user)
			if err != nil {
				t.Fatalf("LinkCapsFor: %v", err)
			}
			if got != tc.want {
				t.Errorf("LinkCapsFor(%s) = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

// LinkCapsForAll must agree with per-ID LinkCapsFor for every role, across a
// mixed set of links (owned, co-owned, shared, unrelated), and return a
// zero-cap entry for every requested ID.
func TestLinkCapsForAll_MatchesPerID(t *testing.T) {
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	ctx := context.Background()

	viewer, err := us.Upsert(ctx, "test", "batch-viewer", "batch-viewer@example.com", "Batch Viewer", "user")
	if err != nil {
		t.Fatalf("seed viewer: %v", err)
	}
	other, err := us.Upsert(ctx, "test", "batch-other", "batch-other@example.com", "Batch Other", "user")
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "batch-admin", "batch-admin@example.com", "Batch Admin", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	owned, err := ls.Create(ctx, "batch-owned", "https://example.com/1", viewer.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed owned: %v", err)
	}
	coowned, err := ls.Create(ctx, "batch-coowned", "https://example.com/2", other.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed coowned: %v", err)
	}
	if err := ls.AddOwner(ctx, coowned.ID, viewer.ID); err != nil {
		t.Fatalf("seed co-ownership: %v", err)
	}
	sharedL, err := ls.Create(ctx, "batch-shared", "https://example.com/3", other.ID, "", "", "secure")
	if err != nil {
		t.Fatalf("seed shared: %v", err)
	}
	if err := ls.AddShare(ctx, sharedL.ID, viewer.ID, other.ID); err != nil {
		t.Fatalf("seed share: %v", err)
	}
	unrelated, err := ls.Create(ctx, "batch-unrelated", "https://example.com/4", other.ID, "", "", "public")
	if err != nil {
		t.Fatalf("seed unrelated: %v", err)
	}

	ids := []string{owned.ID, coowned.ID, sharedL.ID, unrelated.ID}
	for _, tc := range []struct {
		name string
		user *store.User
	}{
		{"viewer", viewer}, {"admin", admin}, {"anonymous", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch, err := store.LinkCapsForAll(ctx, owns, ls, ids, tc.user)
			if err != nil {
				t.Fatalf("LinkCapsForAll: %v", err)
			}
			if len(batch) != len(ids) {
				t.Fatalf("LinkCapsForAll returned %d entries, want %d", len(batch), len(ids))
			}
			for _, id := range ids {
				single, err := store.LinkCapsFor(ctx, owns, ls, id, tc.user)
				if err != nil {
					t.Fatalf("LinkCapsFor(%s): %v", id, err)
				}
				got, ok := batch[id]
				if !ok {
					t.Fatalf("LinkCapsForAll missing entry for %s", id)
				}
				if got != single {
					t.Errorf("caps disagree for %s: batch=%+v single=%+v", id, got, single)
				}
			}
		})
	}

	empty, err := store.LinkCapsForAll(ctx, owns, ls, nil, viewer)
	if err != nil {
		t.Fatalf("LinkCapsForAll(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("LinkCapsForAll(empty) = %d entries, want 0", len(empty))
	}
}
