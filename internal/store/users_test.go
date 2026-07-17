package store_test

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

func TestDeriveDisplayNameSlug(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple name", "Alice Smith", "alice-smith"},
		{"apostrophe and suffix", "Joe O'Brien III", "joe-obrien-iii"},
		{"already lowercase", "alice", "alice"},
		{"leading trailing spaces", "  Bob  ", "bob"},
		{"multiple spaces", "Jane   Doe", "jane-doe"},
		{"special characters", "Test!@#$%User", "testuser"},
		{"consecutive hyphens", "a--b---c", "a-b-c"},
		{"leading trailing hyphens", "-test-", "test"},
		{"empty string", "", ""},
		{"all special chars", "!@#$%^&*()", ""},
		{"unicode letters", "Caf\u00e9 Owner", "caf-owner"},
		{"mixed case", "JoHn DoE", "john-doe"},
		{"dots and commas", "Dr. Jane Smith, PhD", "dr-jane-smith-phd"},
		{"numbers preserved", "User 42", "user-42"},
		{"tabs and newlines", "Tab\tNew\nLine", "tab-new-line"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := store.DeriveDisplayNameSlug(tt.input)
			if got != tt.expected {
				t.Errorf("DeriveDisplayNameSlug(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func newUserStore(t *testing.T) *store.UserStore {
	t.Helper()
	db := testutil.NewTestDB(t)
	return store.NewUserStore(db)
}

func TestGetByDisplayNameSlug(t *testing.T) {
	us := newUserStore(t)
	ctx := context.Background()

	// Create a user via Upsert (which derives and persists the slug).
	u, err := us.Upsert(ctx, "test", "sub1", "alice@example.com", "Alice Smith", "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Lookup by slug should return the same user.
	found, err := us.GetByDisplayNameSlug(ctx, "alice-smith")
	if err != nil {
		t.Fatalf("GetByDisplayNameSlug: %v", err)
	}
	if found.ID != u.ID {
		t.Errorf("expected user ID %s, got %s", u.ID, found.ID)
	}
	if found.DisplayNameSlug != "alice-smith" {
		t.Errorf("expected slug %q, got %q", "alice-smith", found.DisplayNameSlug)
	}

	// Non-existent slug should return error.
	_, err = us.GetByDisplayNameSlug(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent slug, got nil")
	}
	if err != store.ErrNotFound {
		t.Errorf("expected store.ErrNotFound, got %v", err)
	}
}

func TestResolveUniqueSlug_Duplicates(t *testing.T) {
	us := newUserStore(t)
	ctx := context.Background()

	// Create two users with the same display name.
	u1, err := us.Upsert(ctx, "test", "sub1", "alice1@example.com", "Alice Smith", "")
	if err != nil {
		t.Fatalf("upsert user 1: %v", err)
	}
	u2, err := us.Upsert(ctx, "test", "sub2", "alice2@example.com", "Alice Smith", "")
	if err != nil {
		t.Fatalf("upsert user 2: %v", err)
	}

	// First user should get the base slug, second should get a suffix.
	if u1.DisplayNameSlug != "alice-smith" {
		t.Errorf("user 1 slug = %q, want %q", u1.DisplayNameSlug, "alice-smith")
	}
	if u2.DisplayNameSlug != "alice-smith-2" {
		t.Errorf("user 2 slug = %q, want %q", u2.DisplayNameSlug, "alice-smith-2")
	}

	// A third duplicate should get -3.
	u3, err := us.Upsert(ctx, "test", "sub3", "alice3@example.com", "Alice Smith", "")
	if err != nil {
		t.Fatalf("upsert user 3: %v", err)
	}
	if u3.DisplayNameSlug != "alice-smith-3" {
		t.Errorf("user 3 slug = %q, want %q", u3.DisplayNameSlug, "alice-smith-3")
	}
}

func TestUpsert_UpdatesSlugOnNameChange(t *testing.T) {
	us := newUserStore(t)
	ctx := context.Background()

	// Create a user.
	u, err := us.Upsert(ctx, "test", "sub1", "bob@example.com", "Bob Jones", "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if u.DisplayNameSlug != "bob-jones" {
		t.Errorf("initial slug = %q, want %q", u.DisplayNameSlug, "bob-jones")
	}

	// Re-login with a changed display name should update the slug.
	u2, err := us.Upsert(ctx, "test", "sub1", "bob@example.com", "Robert Jones", "")
	if err != nil {
		t.Fatalf("upsert with new name: %v", err)
	}
	if u2.DisplayNameSlug != "robert-jones" {
		t.Errorf("updated slug = %q, want %q", u2.DisplayNameSlug, "robert-jones")
	}
}

func TestUpsert_SpecialCharacterSlug(t *testing.T) {
	us := newUserStore(t)
	ctx := context.Background()

	u, err := us.Upsert(ctx, "test", "sub1", "joe@example.com", "Joe O'Brien III", "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if u.DisplayNameSlug != "joe-obrien-iii" {
		t.Errorf("slug = %q, want %q", u.DisplayNameSlug, "joe-obrien-iii")
	}
}

// newDeleteTestEnv builds the stores DeleteUserWithLinks tests need. The
// shared harness leaves SQLite FK enforcement off, but these tests exercise
// FK-sensitive deletion paths (link_shares.shared_by has no ON DELETE action),
// so the pool is pinned to a single connection with foreign_keys=ON — the same
// enforcement Postgres and MySQL apply unconditionally.
func newDeleteTestEnv(t *testing.T) (*sqlx.DB, *store.UserStore, *store.LinkStore) {
	t.Helper()
	db := testutil.NewTestDB(t)
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	return db, store.NewUserStore(db), store.NewLinkStore(db, owns, tags)
}

func countRows(t *testing.T, db *sqlx.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Get(&n, db.Rebind(query), args...); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// Reassign mode where the admin is already a non-primary co-owner of a link
// the target primarily owns. The ownership transfer previously collided with
// the (link_id, user_id) primary key and rolled back the whole deletion.
func TestDeleteUserWithLinks_ReassignAdminAlreadyCoOwner(t *testing.T) {
	db, us, ls := newDeleteTestEnv(t)
	ctx := context.Background()

	target, err := us.Upsert(ctx, "test", "target", "target@example.com", "Target User", "")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin User", "admin")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	link, err := ls.Create(ctx, "shared-link", "https://example.com", target.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := ls.AddOwner(ctx, link.ID, admin.ID); err != nil {
		t.Fatalf("add admin co-owner: %v", err)
	}

	if err := us.DeleteUserWithLinks(ctx, target.ID, admin.ID, "reassign"); err != nil {
		t.Fatalf("DeleteUserWithLinks reassign: %v", err)
	}

	if n := countRows(t, db, `SELECT COUNT(*) FROM link_owners WHERE link_id = ?`, link.ID); n != 1 {
		t.Errorf("link_owners rows for link = %d, want 1", n)
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_owners WHERE link_id = ? AND user_id = ? AND is_primary = 1`,
		link.ID, admin.ID); n != 1 {
		t.Errorf("admin primary ownership rows = %d, want 1", n)
	}
	if _, err := us.GetByID(ctx, target.ID); err == nil {
		t.Error("target user still exists after deletion")
	}
}

// Reassign mode where the target shared a link with a third user. The
// link_shares.shared_by FK previously blocked the user delete on
// Postgres/MySQL; the share must survive, reattributed to the admin.
func TestDeleteUserWithLinks_ReassignSharesCreatedByTarget(t *testing.T) {
	db, us, ls := newDeleteTestEnv(t)
	ctx := context.Background()

	target, err := us.Upsert(ctx, "test", "target", "target@example.com", "Target User", "")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin User", "admin")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}
	recipient, err := us.Upsert(ctx, "test", "recipient", "recipient@example.com", "Recipient User", "")
	if err != nil {
		t.Fatalf("upsert recipient: %v", err)
	}

	link, err := ls.Create(ctx, "shared-link", "https://example.com", target.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, recipient.ID, target.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	if err := us.DeleteUserWithLinks(ctx, target.ID, admin.ID, "reassign"); err != nil {
		t.Fatalf("DeleteUserWithLinks reassign: %v", err)
	}

	if n := countRows(t, db, `SELECT COUNT(*) FROM link_shares WHERE shared_by = ?`, target.ID); n != 0 {
		t.Errorf("link_shares rows with shared_by=target = %d, want 0", n)
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_shares WHERE link_id = ? AND user_id = ? AND shared_by = ?`,
		link.ID, recipient.ID, admin.ID); n != 1 {
		t.Errorf("recipient share reattributed to admin: rows = %d, want 1", n)
	}
	if _, err := us.GetByID(ctx, target.ID); err == nil {
		t.Error("target user still exists after deletion")
	}
}

// Reassign mode where the target had shared the link with the admin. The
// reattribution would produce a self-share on a link the admin now owns;
// that redundant row must be cleaned up.
func TestDeleteUserWithLinks_ReassignShareToAdminCleaned(t *testing.T) {
	db, us, ls := newDeleteTestEnv(t)
	ctx := context.Background()

	target, err := us.Upsert(ctx, "test", "target", "target@example.com", "Target User", "")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin User", "admin")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	link, err := ls.Create(ctx, "shared-link", "https://example.com", target.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, admin.ID, target.ID); err != nil {
		t.Fatalf("add share to admin: %v", err)
	}

	if err := us.DeleteUserWithLinks(ctx, target.ID, admin.ID, "reassign"); err != nil {
		t.Fatalf("DeleteUserWithLinks reassign: %v", err)
	}

	if n := countRows(t, db, `SELECT COUNT(*) FROM link_shares WHERE link_id = ?`, link.ID); n != 0 {
		t.Errorf("link_shares rows for reassigned link = %d, want 0 (self-share cleaned)", n)
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_owners WHERE link_id = ? AND user_id = ? AND is_primary = 1`,
		link.ID, admin.ID); n != 1 {
		t.Errorf("admin primary ownership rows = %d, want 1", n)
	}
}

// Reassign mode where the target shared with the admin a link the admin
// also co-owns (another user primary). After reattribution the self-share is
// redundant — the admin's co-owner row already grants access — so it must be
// cleaned up even though the admin is not the primary owner.
func TestDeleteUserWithLinks_ReassignShareToAdminOnCoOwnedLink(t *testing.T) {
	db, us, ls := newDeleteTestEnv(t)
	ctx := context.Background()

	owner, err := us.Upsert(ctx, "test", "owner", "owner@example.com", "Owner User", "")
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	target, err := us.Upsert(ctx, "test", "target", "target@example.com", "Target User", "")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin User", "admin")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	link, err := ls.Create(ctx, "co-owned-link", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := ls.AddOwner(ctx, link.ID, target.ID); err != nil {
		t.Fatalf("add target co-owner: %v", err)
	}
	if err := ls.AddOwner(ctx, link.ID, admin.ID); err != nil {
		t.Fatalf("add admin co-owner: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, admin.ID, target.ID); err != nil {
		t.Fatalf("add share to admin: %v", err)
	}

	if err := us.DeleteUserWithLinks(ctx, target.ID, admin.ID, "reassign"); err != nil {
		t.Fatalf("DeleteUserWithLinks reassign: %v", err)
	}

	if n := countRows(t, db, `SELECT COUNT(*) FROM link_shares WHERE link_id = ?`, link.ID); n != 0 {
		t.Errorf("link_shares rows for co-owned link = %d, want 0 (redundant self-share cleaned)", n)
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_owners WHERE link_id = ? AND user_id = ? AND is_primary = 0`,
		link.ID, admin.ID); n != 1 {
		t.Errorf("admin co-owner rows = %d, want 1 (must survive)", n)
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_owners WHERE link_id = ? AND user_id = ? AND is_primary = 1`,
		link.ID, owner.ID); n != 1 {
		t.Errorf("original primary owner rows = %d, want 1", n)
	}
}

// Reassign mode where the target shared with the admin a link the admin does
// not own in any capacity. The reattributed self-share row must be KEPT: it is
// the admin's only access to the link, so deleting it would revoke access.
// The "shared by you" attribution that results is an accepted cosmetic quirk.
func TestDeleteUserWithLinks_ReassignShareToAdminKeptWhenNotOwner(t *testing.T) {
	db, us, ls := newDeleteTestEnv(t)
	ctx := context.Background()

	owner, err := us.Upsert(ctx, "test", "owner", "owner@example.com", "Owner User", "")
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	target, err := us.Upsert(ctx, "test", "target", "target@example.com", "Target User", "")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin User", "admin")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	link, err := ls.Create(ctx, "not-admins-link", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := ls.AddOwner(ctx, link.ID, target.ID); err != nil {
		t.Fatalf("add target co-owner: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, admin.ID, target.ID); err != nil {
		t.Fatalf("add share to admin: %v", err)
	}

	if err := us.DeleteUserWithLinks(ctx, target.ID, admin.ID, "reassign"); err != nil {
		t.Fatalf("DeleteUserWithLinks reassign: %v", err)
	}

	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_shares WHERE link_id = ? AND user_id = ? AND shared_by = ?`,
		link.ID, admin.ID, admin.ID); n != 1 {
		t.Errorf("admin share rows = %d, want 1 (only access to the link, must be kept)", n)
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_owners WHERE link_id = ? AND user_id = ?`,
		link.ID, admin.ID); n != 0 {
		t.Errorf("admin link_owners rows = %d, want 0", n)
	}
	if _, err := us.GetByID(ctx, target.ID); err == nil {
		t.Error("target user still exists after deletion")
	}
}

// Delete mode where the target created a share on a link they merely
// co-owned. The link survives with its primary owner, so the share previously
// kept a dangling shared_by reference that blocked the user delete.
func TestDeleteUserWithLinks_DeleteModeShareOnCoOwnedLink(t *testing.T) {
	db, us, ls := newDeleteTestEnv(t)
	ctx := context.Background()

	owner, err := us.Upsert(ctx, "test", "owner", "owner@example.com", "Owner User", "")
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	target, err := us.Upsert(ctx, "test", "target", "target@example.com", "Target User", "")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin User", "admin")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}
	recipient, err := us.Upsert(ctx, "test", "recipient", "recipient@example.com", "Recipient User", "")
	if err != nil {
		t.Fatalf("upsert recipient: %v", err)
	}

	link, err := ls.Create(ctx, "co-owned-link", "https://example.com", owner.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := ls.AddOwner(ctx, link.ID, target.ID); err != nil {
		t.Fatalf("add target co-owner: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, recipient.ID, target.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	if err := us.DeleteUserWithLinks(ctx, target.ID, admin.ID, "delete"); err != nil {
		t.Fatalf("DeleteUserWithLinks delete: %v", err)
	}

	if _, err := ls.GetByID(ctx, link.ID); err != nil {
		t.Errorf("co-owned link should survive: %v", err)
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_owners WHERE link_id = ? AND user_id = ? AND is_primary = 1`,
		link.ID, owner.ID); n != 1 {
		t.Errorf("owner primary ownership rows = %d, want 1", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM link_owners WHERE user_id = ?`, target.ID); n != 0 {
		t.Errorf("link_owners rows for target = %d, want 0", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM link_shares WHERE shared_by = ?`, target.ID); n != 0 {
		t.Errorf("link_shares rows with shared_by=target = %d, want 0", n)
	}
	if _, err := us.GetByID(ctx, target.ID); err == nil {
		t.Error("target user still exists after deletion")
	}
}

// Delete mode where the target is sole primary owner: the link and its shares
// go away with the user.
func TestDeleteUserWithLinks_DeleteModeSolePrimary(t *testing.T) {
	db, us, ls := newDeleteTestEnv(t)
	ctx := context.Background()

	target, err := us.Upsert(ctx, "test", "target", "target@example.com", "Target User", "")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin User", "admin")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}
	recipient, err := us.Upsert(ctx, "test", "recipient", "recipient@example.com", "Recipient User", "")
	if err != nil {
		t.Fatalf("upsert recipient: %v", err)
	}

	link, err := ls.Create(ctx, "doomed-link", "https://example.com", target.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := ls.AddShare(ctx, link.ID, recipient.ID, target.ID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	if err := us.DeleteUserWithLinks(ctx, target.ID, admin.ID, "delete"); err != nil {
		t.Fatalf("DeleteUserWithLinks delete: %v", err)
	}

	if _, err := ls.GetByID(ctx, link.ID); err == nil {
		t.Error("sole-primary link should be deleted")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM link_shares WHERE link_id = ?`, link.ID); n != 0 {
		t.Errorf("link_shares rows for deleted link = %d, want 0", n)
	}
	if _, err := us.GetByID(ctx, target.ID); err == nil {
		t.Error("target user still exists after deletion")
	}
}

// Plain reassign with no shares and no co-ownership overlap — the path that
// already worked must keep working.
func TestDeleteUserWithLinks_ReassignPlain(t *testing.T) {
	db, us, ls := newDeleteTestEnv(t)
	ctx := context.Background()

	target, err := us.Upsert(ctx, "test", "target", "target@example.com", "Target User", "")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	admin, err := us.Upsert(ctx, "test", "admin", "admin@example.com", "Admin User", "admin")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	link, err := ls.Create(ctx, "plain-link", "https://example.com", target.ID, "", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	if err := us.DeleteUserWithLinks(ctx, target.ID, admin.ID, "reassign"); err != nil {
		t.Fatalf("DeleteUserWithLinks reassign: %v", err)
	}

	if n := countRows(t, db,
		`SELECT COUNT(*) FROM link_owners WHERE link_id = ? AND user_id = ? AND is_primary = 1`,
		link.ID, admin.ID); n != 1 {
		t.Errorf("admin primary ownership rows = %d, want 1", n)
	}
	if _, err := us.GetByID(ctx, target.ID); err == nil {
		t.Error("target user still exists after deletion")
	}
}
