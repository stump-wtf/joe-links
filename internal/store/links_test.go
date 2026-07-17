package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newTestEnv creates a full test environment sharing the same DB.
func newTestEnv(t *testing.T) (*store.LinkStore, *store.TagStore, *store.UserStore, string) {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)

	u, err := us.Upsert(context.Background(), "test", "sub1", "test@example.com", "Test User", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return ls, tags, us, u.ID
}

func TestLinkStore_Create(t *testing.T) {
	ls, _, _, userID := newTestEnv(t)
	ctx := context.Background()

	link, err := ls.Create(ctx, "my-link", "https://example.com", userID, "My Link", "A test link", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.Slug != "my-link" {
		t.Errorf("slug = %q, want %q", link.Slug, "my-link")
	}
	if link.URL != "https://example.com" {
		t.Errorf("url = %q, want %q", link.URL, "https://example.com")
	}
	if link.Title != "My Link" {
		t.Errorf("title = %q, want %q", link.Title, "My Link")
	}
	if link.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestLinkStore_GetBySlug(t *testing.T) {
	ls, _, _, userID := newTestEnv(t)
	ctx := context.Background()

	created, err := ls.Create(ctx, "get-test", "https://example.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := ls.GetBySlug(ctx, "get-test")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
}

func TestLinkStore_GetBySlug_NotFound(t *testing.T) {
	ls, _, _, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := ls.GetBySlug(ctx, "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug(nonexistent) = %v, want ErrNotFound", err)
	}
}

func TestLinkStore_ListAll(t *testing.T) {
	ls, _, _, userID := newTestEnv(t)
	ctx := context.Background()

	_, err := ls.Create(ctx, "aaa-link", "https://a.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = ls.Create(ctx, "bbb-link", "https://b.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	links, err := ls.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("len = %d, want 2", len(links))
	}
	// Should be ordered by slug ASC.
	if links[0].Slug != "aaa-link" {
		t.Errorf("first slug = %q, want %q", links[0].Slug, "aaa-link")
	}
}

func TestLinkStore_ListByOwner(t *testing.T) {
	ls, _, us, userID := newTestEnv(t)
	ctx := context.Background()

	_, err := ls.Create(ctx, "owned-link", "https://example.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Create second user with no links.
	u2, err := us.Upsert(ctx, "test", "sub2", "other@example.com", "Other", "")
	if err != nil {
		t.Fatalf("seed user2: %v", err)
	}

	links, err := ls.ListByOwner(ctx, userID)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("len = %d, want 1", len(links))
	}

	links2, err := ls.ListByOwner(ctx, u2.ID)
	if err != nil {
		t.Fatalf("ListByOwner(user2): %v", err)
	}
	if len(links2) != 0 {
		t.Errorf("user2 links = %d, want 0", len(links2))
	}
}

func TestLinkStore_Update(t *testing.T) {
	ls, _, _, userID := newTestEnv(t)
	ctx := context.Background()

	created, err := ls.Create(ctx, "update-me", "https://old.com", userID, "Old", "Old desc", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := ls.Update(ctx, created.ID, "https://new.com", "New", "New desc", "public")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.URL != "https://new.com" {
		t.Errorf("url = %q, want %q", updated.URL, "https://new.com")
	}
	if updated.Title != "New" {
		t.Errorf("title = %q, want %q", updated.Title, "New")
	}
	if updated.Description != "New desc" {
		t.Errorf("description = %q, want %q", updated.Description, "New desc")
	}
	if updated.UpdatedAt.Before(created.CreatedAt) {
		t.Errorf("UpdatedAt %v should not be before CreatedAt %v", updated.UpdatedAt, created.CreatedAt)
	}
}

func TestLinkStore_Delete(t *testing.T) {
	ls, _, _, userID := newTestEnv(t)
	ctx := context.Background()

	created, err := ls.Create(ctx, "delete-me", "https://example.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = ls.Delete(ctx, created.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = ls.GetBySlug(ctx, "delete-me")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug after delete = %v, want ErrNotFound", err)
	}
}

func TestLinkStore_SlugUniqueness(t *testing.T) {
	ls, _, _, userID := newTestEnv(t)
	ctx := context.Background()

	_, err := ls.Create(ctx, "unique-slug", "https://a.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}

	_, err = ls.Create(ctx, "unique-slug", "https://b.com", userID, "", "", "")
	if !errors.Is(err, store.ErrSlugTaken) {
		t.Errorf("Create duplicate slug = %v, want ErrSlugTaken", err)
	}
}

func TestLinkStore_GetByID_NotFound(t *testing.T) {
	ls, _, _, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := ls.GetByID(ctx, "nonexistent-id")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByID(nonexistent) = %v, want ErrNotFound", err)
	}
}

func TestLinkStore_SetAndListTags(t *testing.T) {
	ls, _, _, userID := newTestEnv(t)
	ctx := context.Background()

	link, err := ls.Create(ctx, "tagged-link", "https://example.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = ls.SetTags(ctx, link.ID, []string{"go", "tools"})
	if err != nil {
		t.Fatalf("SetTags: %v", err)
	}

	tags, err := ls.ListTags(ctx, link.ID)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("len(tags) = %d, want 2", len(tags))
	}
}

func TestLinkStore_ListByTag(t *testing.T) {
	ls, _, _, userID := newTestEnv(t)
	ctx := context.Background()

	link, err := ls.Create(ctx, "tag-filter", "https://example.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = ls.SetTags(ctx, link.ID, []string{"golang"})
	if err != nil {
		t.Fatalf("SetTags: %v", err)
	}

	links, err := ls.ListByTag(ctx, "golang")
	if err != nil {
		t.Fatalf("ListByTag: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("len = %d, want 1", len(links))
	}
	if links[0].Slug != "tag-filter" {
		t.Errorf("slug = %q, want %q", links[0].Slug, "tag-filter")
	}
}

// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — tag pages must
// only surface links the viewer may see (issue #193).
func TestLinkStore_ListVisibleByTag(t *testing.T) {
	ls, _, us, ownerID := newTestEnv(t)
	ctx := context.Background()

	viewer, err := us.Upsert(ctx, "test", "sub-viewer", "viewer@example.com", "Viewer", "")
	if err != nil {
		t.Fatalf("seed viewer: %v", err)
	}

	// Owner's links, all tagged "finance".
	seed := func(slug, visibility string) *store.Link {
		t.Helper()
		l, err := ls.Create(ctx, slug, "https://example.com/"+slug, ownerID, "", "", visibility)
		if err != nil {
			t.Fatalf("Create %q: %v", slug, err)
		}
		if err := ls.SetTags(ctx, l.ID, []string{"finance"}); err != nil {
			t.Fatalf("SetTags %q: %v", slug, err)
		}
		return l
	}
	seed("fin-public", "public")
	seed("fin-private", "private")
	secure := seed("fin-secure", "secure")

	slugs := func(links []*store.Link) []string {
		out := make([]string, 0, len(links))
		for _, l := range links {
			out = append(out, l.Slug)
		}
		return out
	}

	// (a)+(b)+(c) — another user sees the public link only.
	got, err := ls.ListVisibleByTag(ctx, "finance", viewer.ID)
	if err != nil {
		t.Fatalf("ListVisibleByTag(viewer): %v", err)
	}
	if len(got) != 1 || got[0].Slug != "fin-public" {
		t.Fatalf("viewer sees %v, want [fin-public] only", slugs(got))
	}

	// Anonymous viewers (empty userID) also see public only.
	got, err = ls.ListVisibleByTag(ctx, "finance", "")
	if err != nil {
		t.Fatalf("ListVisibleByTag(anonymous): %v", err)
	}
	if len(got) != 1 || got[0].Slug != "fin-public" {
		t.Fatalf("anonymous sees %v, want [fin-public] only", slugs(got))
	}

	// (c) — sharing the secure link makes it visible to the viewer.
	if err := ls.AddShare(ctx, secure.ID, viewer.ID, ownerID); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	got, err = ls.ListVisibleByTag(ctx, "finance", viewer.ID)
	if err != nil {
		t.Fatalf("ListVisibleByTag(viewer, shared): %v", err)
	}
	if len(got) != 2 || got[0].Slug != "fin-public" || got[1].Slug != "fin-secure" {
		t.Fatalf("viewer with share sees %v, want [fin-public fin-secure]", slugs(got))
	}

	// (d) — the owner sees all of their own links regardless of visibility.
	got, err = ls.ListVisibleByTag(ctx, "finance", ownerID)
	if err != nil {
		t.Fatalf("ListVisibleByTag(owner): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("owner sees %v, want all 3", slugs(got))
	}

	// (d) — a co-owner also sees the private/secure links.
	coOwner, err := us.Upsert(ctx, "test", "sub-coowner", "coowner@example.com", "CoOwner", "")
	if err != nil {
		t.Fatalf("seed co-owner: %v", err)
	}
	for _, l := range got {
		if l.Slug == "fin-private" {
			if err := ls.AddOwner(ctx, l.ID, coOwner.ID); err != nil {
				t.Fatalf("AddOwner: %v", err)
			}
		}
	}
	coGot, err := ls.ListVisibleByTag(ctx, "finance", coOwner.ID)
	if err != nil {
		t.Fatalf("ListVisibleByTag(co-owner): %v", err)
	}
	if len(coGot) != 2 || coGot[0].Slug != "fin-private" || coGot[1].Slug != "fin-public" {
		t.Fatalf("co-owner sees %v, want [fin-private fin-public]", slugs(coGot))
	}

	// (e) — viewer who is both owner and share recipient gets no duplicate rows.
	if err := ls.AddShare(ctx, secure.ID, ownerID, ownerID); err != nil {
		t.Fatalf("AddShare(owner): %v", err)
	}
	got, err = ls.ListVisibleByTag(ctx, "finance", ownerID)
	if err != nil {
		t.Fatalf("ListVisibleByTag(owner, self-share): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("owner with self-share sees %v, want 3 distinct rows", slugs(got))
	}
}

// Governing: SPEC-0002 REQ "Multi-Ownership via link_owners" — created_at column populated on insert.
func TestLinkStore_Create_OwnerCreatedAt(t *testing.T) {
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	ctx := context.Background()

	u, err := us.Upsert(ctx, "test", "sub1", "owner@example.com", "Owner", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	link, err := ls.Create(ctx, "ts-link", "https://example.com", u.ID, "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var createdAt sql.NullTime
	if err := db.QueryRowx(
		db.Rebind(`SELECT created_at FROM link_owners WHERE link_id = ? AND user_id = ?`),
		link.ID, u.ID,
	).Scan(&createdAt); err != nil {
		t.Fatalf("query created_at: %v", err)
	}
	if !createdAt.Valid || createdAt.Time.IsZero() {
		t.Errorf("link_owners.created_at = %v, want a non-zero timestamp", createdAt)
	}
}
