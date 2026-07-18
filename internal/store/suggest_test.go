// Store-layer tests for the SPEC-0019 suggest query: visibility matrix,
// ranking bands, LIKE-wildcard neutralization, lifecycle exclusion, and the
// query/limit bounds. Tests are named after the SPEC-0019 REQ "Suggest
// Endpoint" scenarios where one applies, so the spec↔test mapping is
// auditable.
//
// Governing: SPEC-0019 REQ "Suggest Endpoint", ADR-0019
package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newSuggestEnv builds a link store plus the raw DB handle (for lifecycle
// seeding) and two users: the viewer and another user whose links exercise
// the visibility filter.
func newSuggestEnv(t *testing.T) (ls *store.LinkStore, db *sqlx.DB, viewerID, otherID string) {
	t.Helper()
	db = testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls = store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)

	viewer, err := us.Upsert(context.Background(), "test", "sub-viewer", "viewer@example.com", "Viewer", "")
	if err != nil {
		t.Fatalf("seed viewer: %v", err)
	}
	other, err := us.Upsert(context.Background(), "test", "sub-other", "other@example.com", "Other", "")
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}
	return ls, db, viewer.ID, other.ID
}

// mustCreate creates a link with the given slug/owner/visibility, failing the
// test on error.
func mustCreate(t *testing.T, ls *store.LinkStore, slug, ownerID, title, description, visibility string) *store.Link {
	t.Helper()
	l, err := ls.Create(context.Background(), slug, "https://example.com", ownerID, title, description, visibility)
	if err != nil {
		t.Fatalf("create %q: %v", slug, err)
	}
	return l
}

// archiveLinkRaw sets archived_at via raw SQL (there is no archive write path
// in this story's scope).
func archiveLinkRaw(t *testing.T, db *sqlx.DB, linkID string) {
	t.Helper()
	if _, err := db.Exec(db.Rebind(`UPDATE links SET archived_at = ? WHERE id = ?`),
		time.Now().UTC().Truncate(time.Second), linkID); err != nil {
		t.Fatalf("archive link: %v", err)
	}
}

// slugs extracts the slugs from a suggest result, in order.
func slugs(links []*store.Link) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.Slug
	}
	return out
}

func assertSlugs(t *testing.T, got []*store.Link, want ...string) {
	t.Helper()
	g := slugs(got)
	if len(g) != len(want) {
		t.Fatalf("got slugs %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("got slugs %v, want %v", g, want)
		}
	}
}

// TestSuggestLinks_VisibilityMatrix is the authz matrix behind scenarios
// "Other Users' Private Links Excluded" and "Shared Secure Link Included":
// a non-admin viewer is offered public links, their own private links,
// co-owned private links, and secure links shared with them — and never
// another user's private or secure links. An admin is offered everything
// (SPEC-0010 REQ "Admin Visibility Override").
func TestSuggestLinks_VisibilityMatrix(t *testing.T) {
	ls, _, viewerID, otherID := newSuggestEnv(t)
	ctx := context.Background()

	mustCreate(t, ls, "zz-pub", otherID, "", "", "public")
	mustCreate(t, ls, "zz-own-priv", viewerID, "", "", "private")
	coOwned := mustCreate(t, ls, "zz-co-priv", otherID, "", "", "private")
	sharedSec := mustCreate(t, ls, "zz-shared-sec", otherID, "", "", "secure")
	mustCreate(t, ls, "zz-priv", otherID, "", "", "private")
	mustCreate(t, ls, "zz-sec", otherID, "", "", "secure")

	if err := ls.AddOwner(ctx, coOwned.ID, viewerID); err != nil {
		t.Fatalf("add co-owner: %v", err)
	}
	if err := ls.AddShare(ctx, sharedSec.ID, viewerID, otherID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	got, err := ls.SuggestLinks(ctx, viewerID, false, "zz", 10)
	if err != nil {
		t.Fatalf("SuggestLinks (non-admin): %v", err)
	}
	// All matches are slug-prefix band; within the band, slug ascending.
	assertSlugs(t, got, "zz-co-priv", "zz-own-priv", "zz-pub", "zz-shared-sec")

	admin, err := ls.SuggestLinks(ctx, viewerID, true, "zz", 10)
	if err != nil {
		t.Fatalf("SuggestLinks (admin): %v", err)
	}
	assertSlugs(t, admin, "zz-co-priv", "zz-own-priv", "zz-priv", "zz-pub", "zz-sec", "zz-shared-sec")
}

// Scenario: Prefix Match Ranks First — slug-prefix matches rank before
// slug-substring matches, which rank before title/description matches.
func TestSuggestLinks_PrefixMatchRanksFirst(t *testing.T) {
	ls, _, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	// Created deliberately out of rank order.
	mustCreate(t, ls, "docs", viewerID, "Jira runbook", "", "public")
	mustCreate(t, ls, "fiji-trip", viewerID, "", "", "public")
	mustCreate(t, ls, "jira", viewerID, "", "", "public")

	got, err := ls.SuggestLinks(ctx, viewerID, false, "ji", 10)
	if err != nil {
		t.Fatalf("SuggestLinks: %v", err)
	}
	assertSlugs(t, got, "jira", "fiji-trip", "docs")
}

// Scenario: LIKE Wildcards Neutralized — % and _ in q match literally, never
// as SQL wildcards.
func TestSuggestLinks_LikeWildcardsNeutralized(t *testing.T) {
	ls, _, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	mustCreate(t, ls, "jira", viewerID, "", "", "public")
	mustCreate(t, ls, "pct", viewerID, "100% done", "", "public")
	mustCreate(t, ls, "under", viewerID, "", "snake_case naming", "public")

	// A bare % as a wildcard would match every link; literally it matches
	// only the title containing a percent sign.
	got, err := ls.SuggestLinks(ctx, viewerID, false, "%", 10)
	if err != nil {
		t.Fatalf("SuggestLinks(%%): %v", err)
	}
	assertSlugs(t, got, "pct")

	// A bare _ as a wildcard would match any single character; literally it
	// matches only the description containing an underscore.
	got, err = ls.SuggestLinks(ctx, viewerID, false, "_", 10)
	if err != nil {
		t.Fatalf("SuggestLinks(_): %v", err)
	}
	assertSlugs(t, got, "under")
}

// Expired and archived links are excluded from the candidate set for all
// callers, admin included (SPEC-0019 REQ "Suggest Endpoint", SPEC-0020).
func TestSuggestLinks_ExpiredAndArchivedExcluded(t *testing.T) {
	ls, db, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	mustCreate(t, ls, "life-active", viewerID, "", "", "public")
	expired := mustCreate(t, ls, "life-expired", viewerID, "", "", "public")
	archived := mustCreate(t, ls, "life-archived", viewerID, "", "", "public")

	backdateExpiry(t, db, expired.ID, time.Now().UTC().Add(-time.Hour))
	archiveLinkRaw(t, db, archived.ID)

	got, err := ls.SuggestLinks(ctx, viewerID, false, "life", 10)
	if err != nil {
		t.Fatalf("SuggestLinks (non-admin): %v", err)
	}
	assertSlugs(t, got, "life-active")

	admin, err := ls.SuggestLinks(ctx, viewerID, true, "life", 10)
	if err != nil {
		t.Fatalf("SuggestLinks (admin): %v", err)
	}
	assertSlugs(t, admin, "life-active")
}

// q is lowercased before matching (the slug corpus is canonically lowercase)
// and truncated to MaxSuggestQueryLen characters.
func TestSuggestLinks_QueryLowercasedAndTruncated(t *testing.T) {
	ls, _, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	mustCreate(t, ls, "jira", viewerID, "", "", "public")
	mustCreate(t, ls, "trunc", viewerID, strings.Repeat("a", store.MaxSuggestQueryLen), "", "public")

	got, err := ls.SuggestLinks(ctx, viewerID, false, "JI", 10)
	if err != nil {
		t.Fatalf("SuggestLinks(JI): %v", err)
	}
	assertSlugs(t, got, "jira")

	// A 70-character query is truncated to 64: the truncated form matches the
	// 64-character title, while the untruncated form could not.
	got, err = ls.SuggestLinks(ctx, viewerID, false, strings.Repeat("A", store.MaxSuggestQueryLen+6), 10)
	if err != nil {
		t.Fatalf("SuggestLinks(long): %v", err)
	}
	assertSlugs(t, got, "trunc")
}

// An empty q returns an empty suggestions set, never the whole corpus.
func TestSuggestLinks_EmptyQueryReturnsNoSuggestions(t *testing.T) {
	ls, _, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	mustCreate(t, ls, "jira", viewerID, "", "", "public")

	got, err := ls.SuggestLinks(ctx, viewerID, false, "", 10)
	if err != nil {
		t.Fatalf("SuggestLinks(\"\"): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("SuggestLinks(\"\") = %v, want empty non-nil slice", slugs(got))
	}
}

// Result count bounds: default 5 when limit is unset/non-positive, explicit
// limits honored up to 10, higher values clamped to 10.
func TestSuggestLinks_LimitDefaultsAndClamps(t *testing.T) {
	ls, _, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		mustCreate(t, ls, fmt.Sprintf("lim-%02d", i), viewerID, "", "", "public")
	}

	cases := []struct {
		limit int
		want  int
	}{
		{limit: 0, want: store.DefaultSuggestLimit},
		{limit: -1, want: store.DefaultSuggestLimit},
		{limit: 7, want: 7},
		{limit: 99, want: store.MaxSuggestLimit},
	}
	for _, tc := range cases {
		got, err := ls.SuggestLinks(ctx, viewerID, false, "lim", tc.limit)
		if err != nil {
			t.Fatalf("SuggestLinks(limit=%d): %v", tc.limit, err)
		}
		if len(got) != tc.want {
			t.Errorf("SuggestLinks(limit=%d) returned %d results, want %d", tc.limit, len(got), tc.want)
		}
	}
}
