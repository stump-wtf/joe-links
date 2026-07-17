package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newAdminSearchEnv seeds two users so owner-filter searches can distinguish
// "matches one owner" from "returns all owners".
func newAdminSearchEnv(t *testing.T) (*store.LinkStore, string, string) {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)

	ctx := context.Background()
	alice, err := us.Upsert(ctx, "test", "sub-alice", "alice@example.com", "Alice Adams", "")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	bob, err := us.Upsert(ctx, "test", "sub-bob", "bob@example.com", "Bob Builder", "")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	return ls, alice.ID, bob.ID
}

// TestLinkStore_ListAllAdmin_OwnerFilterKeepsFullAggregates guards against the
// pre-#205 bug where the owner-name filter was a WHERE on the joined
// users.display_name: GROUP_CONCAT then only saw the matching owner row, so
// the aggregated owners (and tags) columns were truncated to the matching
// subset. The EXISTS-subquery rewrite must return the FULL owner and tag lists.
func TestLinkStore_ListAllAdmin_OwnerFilterKeepsFullAggregates(t *testing.T) {
	ls, aliceID, bobID := newAdminSearchEnv(t)
	ctx := context.Background()

	link, err := ls.Create(ctx, "shared-link", "https://shared.example.com", aliceID, "Shared", "", "")
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if err := ls.AddOwner(ctx, link.ID, bobID); err != nil {
		t.Fatalf("add co-owner: %v", err)
	}
	if err := ls.SetTags(ctx, link.ID, []string{"alpha", "beta"}); err != nil {
		t.Fatalf("set tags: %v", err)
	}
	// A second link owned only by Alice must NOT match a "Bob" search.
	if _, err := ls.Create(ctx, "alice-only", "https://alice.example.com", aliceID, "", "", ""); err != nil {
		t.Fatalf("create alice-only link: %v", err)
	}

	results, err := ls.ListAllAdmin(ctx, "Bob")
	if err != nil {
		t.Fatalf("ListAllAdmin: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (only the Bob-co-owned link)", len(results))
	}
	got := results[0]
	if got.Slug != "shared-link" {
		t.Fatalf("slug = %q, want %q", got.Slug, "shared-link")
	}
	for _, owner := range []string{"Alice Adams", "Bob Builder"} {
		if !strings.Contains(got.Owners, owner) {
			t.Errorf("owners = %q, want it to contain %q (aggregate truncated by filter?)", got.Owners, owner)
		}
	}
	for _, tag := range []string{"alpha", "beta"} {
		if !strings.Contains(got.Tags, tag) {
			t.Errorf("tags = %q, want it to contain %q (aggregate truncated by filter?)", got.Tags, tag)
		}
	}
}

// TestLinkStore_Search_CaseInsensitiveLike exercises the LOWER(col) LIKE
// LOWER(?) query shape in SearchAll, SearchByOwner, ListAllAdmin, and
// ListPublic. SQLite's LIKE already folds ASCII case, so on this backend the
// test primarily proves the rewritten queries are valid and still match; the
// LOWER() wrapping exists for PostgreSQL, where plain LIKE is case-sensitive
// and an uppercase query would otherwise return nothing.
func TestLinkStore_Search_CaseInsensitiveLike(t *testing.T) {
	ls, aliceID, _ := newAdminSearchEnv(t)
	ctx := context.Background()

	if _, err := ls.Create(ctx, "case-link", "https://example.com/casetest", aliceID, "Case Title", "case description", "public"); err != nil {
		t.Fatalf("create link: %v", err)
	}

	t.Run("SearchAll", func(t *testing.T) {
		links, err := ls.SearchAll(ctx, "CASETEST")
		if err != nil {
			t.Fatalf("SearchAll: %v", err)
		}
		if len(links) != 1 {
			t.Errorf("len = %d, want 1", len(links))
		}
	})

	t.Run("SearchByOwner", func(t *testing.T) {
		links, err := ls.SearchByOwner(ctx, aliceID, "CASE-LINK")
		if err != nil {
			t.Fatalf("SearchByOwner: %v", err)
		}
		if len(links) != 1 {
			t.Errorf("len = %d, want 1", len(links))
		}
	})

	t.Run("ListAllAdmin", func(t *testing.T) {
		links, err := ls.ListAllAdmin(ctx, "ALICE ADAMS")
		if err != nil {
			t.Fatalf("ListAllAdmin: %v", err)
		}
		if len(links) != 1 {
			t.Errorf("len = %d, want 1", len(links))
		}
	})

	t.Run("ListPublic", func(t *testing.T) {
		links, total, err := ls.ListPublic(ctx, "", "CASE TITLE", 1, 10)
		if err != nil {
			t.Fatalf("ListPublic: %v", err)
		}
		if total != 1 || len(links) != 1 {
			t.Errorf("total = %d, len = %d, want 1/1", total, len(links))
		}
	})
}
