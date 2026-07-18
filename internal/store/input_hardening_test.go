// Store-level input hardening (issues #251, #265): the tag upsert pair and the
// link write methods are the single choke points every surface funnels
// through, so hostile tag names, non-http(s) URLs, and oversized tag lists
// must be rejected here even if a future caller skips handler validation —
// and user-supplied search terms must match LIKE metacharacters literally.
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newHardeningEnv wires the store stack over an in-memory SQLite database.
func newHardeningEnv(t *testing.T) (*store.LinkStore, *store.TagStore, string) {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)

	u, err := us.Upsert(context.Background(), "test", "sub-hardening", "hardening@example.com", "Hardening", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return ls, tags, u.ID
}

// --- tag-name intake backstop (issue #251) --------------------------------

func TestTagStore_Upsert_RejectsHostileNames(t *testing.T) {
	_, tags, _ := newHardeningEnv(t)
	ctx := context.Background()

	for _, name := range []string{
		`x');fetch('/evil')//`, // the enabling payload from the PR #241 review
		`<script>alert(1)</script>`,
		"日本語",          // would derive an empty slug
		`'';!--"<XSS>`, // would derive an empty slug
		"",
	} {
		if _, err := tags.Upsert(ctx, name); !errors.Is(err, store.ErrTagNameInvalid) {
			t.Errorf("Upsert(%q) = %v, want ErrTagNameInvalid", name, err)
		}
	}

	// Valid names still work, and derive non-empty slugs.
	tag, err := tags.Upsert(ctx, "My Tag")
	if err != nil {
		t.Fatalf("Upsert valid name: %v", err)
	}
	if tag.Slug == "" {
		t.Errorf("Upsert(%q) derived empty slug", "My Tag")
	}
}

// SetTags funnels through upsertTx — the transactional backstop — and
// validates the whole list before touching any row, so a rejected write
// leaves the link's existing tags intact.
func TestLinkStore_SetTags_RejectsHostileNamesWithoutClearingTags(t *testing.T) {
	ls, _, userID := newHardeningEnv(t)
	ctx := context.Background()

	link, err := ls.Create(ctx, "tagged", "https://example.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := ls.SetTags(ctx, link.ID, []string{"keep-me"}); err != nil {
		t.Fatalf("seed tags: %v", err)
	}

	err = ls.SetTags(ctx, link.ID, []string{`x');fetch('/evil')//`})
	if !errors.Is(err, store.ErrTagNameInvalid) {
		t.Fatalf("SetTags hostile = %v, want ErrTagNameInvalid", err)
	}

	tags, err := ls.ListTags(ctx, link.ID)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Name != "keep-me" {
		t.Errorf("tags after rejected SetTags = %+v, want the original keep-me tag", tags)
	}
}

func TestLinkStore_SetTags_RejectsTooManyTags(t *testing.T) {
	ls, _, userID := newHardeningEnv(t)
	ctx := context.Background()

	link, err := ls.Create(ctx, "capped", "https://example.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	over := make([]string, store.MaxTagsPerLink+1)
	for i := range over {
		over[i] = "tag-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if err := ls.SetTags(ctx, link.ID, over); !errors.Is(err, store.ErrTooManyTags) {
		t.Errorf("SetTags(%d tags) = %v, want ErrTooManyTags", len(over), err)
	}
}

func TestLinkStore_CreateFull_RejectsHostileTagName_NoHalfCreatedLink(t *testing.T) {
	ls, _, userID := newHardeningEnv(t)
	ctx := context.Background()

	_, err := ls.CreateFull(ctx, "doomed", "https://example.com", userID, "", "", "public",
		[]string{`x');fetch('/evil')//`}, nil, "")
	if !errors.Is(err, store.ErrTagNameInvalid) {
		t.Fatalf("CreateFull hostile tag = %v, want ErrTagNameInvalid", err)
	}
	if _, err := ls.GetBySlug(ctx, "doomed"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("link exists after rejected CreateFull, GetBySlug err = %v", err)
	}
}

// --- URL scheme allowlist backstop (issue #265) ---------------------------

func TestLinkStore_WritePaths_RejectNonHTTPSchemes(t *testing.T) {
	ls, _, userID := newHardeningEnv(t)
	ctx := context.Background()

	link, err := ls.Create(ctx, "good", "https://example.com", userID, "", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	for _, badURL := range []string{"javascript:alert(1)", "data:text/html,x", "//evil.example.com"} {
		if _, err := ls.Create(ctx, "bad-create", badURL, userID, "", "", ""); !errors.Is(err, store.ErrURLSchemeInvalid) {
			t.Errorf("Create(%q) = %v, want ErrURLSchemeInvalid", badURL, err)
		}
		if _, err := ls.CreateFull(ctx, "bad-createfull", badURL, userID, "", "", "public", nil, nil, ""); !errors.Is(err, store.ErrURLSchemeInvalid) {
			t.Errorf("CreateFull(%q) = %v, want ErrURLSchemeInvalid", badURL, err)
		}
		if _, err := ls.Update(ctx, link.ID, badURL, "", "", "public"); !errors.Is(err, store.ErrURLSchemeInvalid) {
			t.Errorf("Update(%q) = %v, want ErrURLSchemeInvalid", badURL, err)
		}
	}

	// The link is untouched by the rejected updates.
	got, err := ls.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("reload link: %v", err)
	}
	if got.URL != "https://example.com" {
		t.Errorf("url = %q, want unchanged https://example.com", got.URL)
	}
}

// --- LIKE wildcard escaping (issue #265) ----------------------------------

// seedSearchCorpus creates three public links whose titles/descriptions
// distinguish literal-metacharacter matches from wildcard matches. The
// metacharacter text rides in BOTH title and description because the search
// arms cover different column sets (ListAllAdmin matches title, not
// description).
func seedSearchCorpus(t *testing.T, ls *store.LinkStore, userID string) {
	t.Helper()
	ctx := context.Background()
	for slug, text := range map[string]string{
		"percent": "100% legit discount",
		"snake":   "snake_case docs",
		"plain":   "snakeXcase plain docs",
	} {
		if _, err := ls.Create(ctx, slug, "https://example.com/"+slug, userID, text, text, "public"); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
}

func TestLinkStore_Search_EscapesLikeWildcards(t *testing.T) {
	ls, _, userID := newHardeningEnv(t)
	ctx := context.Background()
	seedSearchCorpus(t, ls, userID)

	t.Run("SearchAll percent is literal", func(t *testing.T) {
		got, err := ls.SearchAll(ctx, "100%")
		if err != nil {
			t.Fatalf("SearchAll: %v", err)
		}
		if len(got) != 1 || got[0].Slug != "percent" {
			t.Errorf("SearchAll(%q) = %d rows, want only the literal %% match", "100%", len(got))
		}
	})

	t.Run("SearchAll underscore is literal", func(t *testing.T) {
		got, err := ls.SearchAll(ctx, "snake_case")
		if err != nil {
			t.Fatalf("SearchAll: %v", err)
		}
		if len(got) != 1 || got[0].Slug != "snake" {
			t.Errorf("SearchAll(%q) matched %d rows, want 1 (\"_\" must not match X)", "snake_case", len(got))
		}
	})

	t.Run("SearchAll bare percent matches only literal percents", func(t *testing.T) {
		got, err := ls.SearchAll(ctx, "%")
		if err != nil {
			t.Fatalf("SearchAll: %v", err)
		}
		if len(got) != 1 || got[0].Slug != "percent" {
			t.Errorf("SearchAll(%q) = %d rows, want 1 — bare %% must not match everything", "%", len(got))
		}
	})

	t.Run("SearchByOwner underscore is literal", func(t *testing.T) {
		got, err := ls.SearchByOwner(ctx, userID, "snake_case")
		if err != nil {
			t.Fatalf("SearchByOwner: %v", err)
		}
		if len(got) != 1 || got[0].Slug != "snake" {
			t.Errorf("SearchByOwner(%q) matched %d rows, want 1", "snake_case", len(got))
		}
	})

	t.Run("ListAllAdmin percent is literal", func(t *testing.T) {
		got, err := ls.ListAllAdmin(ctx, "100%")
		if err != nil {
			t.Fatalf("ListAllAdmin: %v", err)
		}
		if len(got) != 1 || got[0].Slug != "percent" {
			t.Errorf("ListAllAdmin(%q) = %d rows, want 1", "100%", len(got))
		}
	})

	t.Run("ListPublic underscore is literal", func(t *testing.T) {
		got, total, err := ls.ListPublic(ctx, "", "snake_case", 1, 50)
		if err != nil {
			t.Fatalf("ListPublic: %v", err)
		}
		if total != 1 || len(got) != 1 || got[0].Slug != "snake" {
			t.Errorf("ListPublic(%q) = %d rows (total %d), want 1", "snake_case", len(got), total)
		}
	})
}

func TestTagStore_SearchByPrefix_EscapesLikeWildcards(t *testing.T) {
	ls, tags, userID := newHardeningEnv(t)
	ctx := context.Background()

	link, err := ls.Create(ctx, "tagged", "https://example.com", userID, "", "", "public")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := ls.SetTags(ctx, link.ID, []string{"x_y", "xay"}); err != nil {
		t.Fatalf("seed tags: %v", err)
	}

	got, err := tags.SearchByPrefix(ctx, "x_")
	if err != nil {
		t.Fatalf("SearchByPrefix: %v", err)
	}
	if len(got) != 1 || got[0].Name != "x_y" {
		t.Errorf("SearchByPrefix(%q) = %+v, want only the literal x_y tag", "x_", got)
	}

	// The visibility-filtered arm used by the autocomplete endpoint must
	// escape identically (anonymous viewer, public link).
	got, err = tags.SearchByPrefixVisible(ctx, "x_", "")
	if err != nil {
		t.Fatalf("SearchByPrefixVisible: %v", err)
	}
	if len(got) != 1 || got[0].Name != "x_y" {
		t.Errorf("SearchByPrefixVisible(%q) = %+v, want only the literal x_y tag", "x_", got)
	}
}
