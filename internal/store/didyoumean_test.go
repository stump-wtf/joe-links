// Store-layer tests for the SPEC-0019 did-you-mean candidate query: the SQL
// length window, the viewer-visibility matrix (including the anonymous
// empty-viewerID arm), and the lifecycle exclusion. Distance bounding,
// ordering, and the 3-result cap are handler concerns tested in
// internal/handler/resolve_didyoumean_test.go.
//
// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions", ADR-0019
package store_test

import (
	"context"
	"testing"
	"time"
)

func assertStrings(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Only slugs whose length is within ±2 of the query can be within Levenshtein
// distance 2, so the SQL window excludes everything else before any distance
// is computed.
func TestDidYouMeanCandidates_LengthWindowBoundsCandidates(t *testing.T) {
	ls, _, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	for _, slug := range []string{"ab", "abc", "abcdefg", "abcdefgh"} {
		mustCreate(t, ls, slug, viewerID, "", "", "public")
	}

	// q "abcde" (5) → window 3..7: "ab" (2) and "abcdefgh" (8) are excluded.
	got, err := ls.DidYouMeanCandidates(ctx, "", false, "abcde")
	if err != nil {
		t.Fatalf("DidYouMeanCandidates: %v", err)
	}
	assertStrings(t, got, "abc", "abcdefg")
}

// The visibility matrix behind the "Private Slug Existence Not Leaked" and
// "Owner Sees Their Own Private Slug Suggested" scenarios: anonymous viewers
// (empty viewerID) are offered public slugs only; authenticated non-admins
// add their own/co-owned links and links shared with them; admins get all.
// Discoverability, not resolvability, is the governing test — a private link
// would resolve if the exact slug were known, but must not be discoverable.
func TestDidYouMeanCandidates_VisibilityMatrix(t *testing.T) {
	ls, _, viewerID, otherID := newSuggestEnv(t)
	ctx := context.Background()

	mustCreate(t, ls, "vm-pub", otherID, "", "", "public")
	mustCreate(t, ls, "vm-own", viewerID, "", "", "private")
	coOwned := mustCreate(t, ls, "vm-cow", otherID, "", "", "private")
	shared := mustCreate(t, ls, "vm-shr", otherID, "", "", "secure")
	mustCreate(t, ls, "vm-prv", otherID, "", "", "private")
	mustCreate(t, ls, "vm-sec", otherID, "", "", "secure")

	if err := ls.AddOwner(ctx, coOwned.ID, viewerID); err != nil {
		t.Fatalf("add co-owner: %v", err)
	}
	if err := ls.AddShare(ctx, shared.ID, viewerID, otherID); err != nil {
		t.Fatalf("add share: %v", err)
	}

	// All six slugs are length 6, inside the window for a length-6 query.
	anon, err := ls.DidYouMeanCandidates(ctx, "", false, "vm-xxx")
	if err != nil {
		t.Fatalf("DidYouMeanCandidates (anonymous): %v", err)
	}
	assertStrings(t, anon, "vm-pub")

	viewer, err := ls.DidYouMeanCandidates(ctx, viewerID, false, "vm-xxx")
	if err != nil {
		t.Fatalf("DidYouMeanCandidates (viewer): %v", err)
	}
	assertStrings(t, viewer, "vm-cow", "vm-own", "vm-pub", "vm-shr")

	admin, err := ls.DidYouMeanCandidates(ctx, viewerID, true, "vm-xxx")
	if err != nil {
		t.Fatalf("DidYouMeanCandidates (admin): %v", err)
	}
	assertStrings(t, admin, "vm-cow", "vm-own", "vm-prv", "vm-pub", "vm-sec", "vm-shr")
}

// Expired and archived links are excluded from the candidate set for all
// callers, admin included — suggesting a link that will not resolve is worse
// than no suggestion (SPEC-0020).
func TestDidYouMeanCandidates_ExpiredAndArchivedExcluded(t *testing.T) {
	ls, db, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	mustCreate(t, ls, "dym-act", viewerID, "", "", "public")
	expired := mustCreate(t, ls, "dym-exp", viewerID, "", "", "public")
	archived := mustCreate(t, ls, "dym-arc", viewerID, "", "", "public")

	backdateExpiry(t, db, expired.ID, time.Now().UTC().Add(-time.Hour))
	archiveLinkRaw(t, db, archived.ID)

	got, err := ls.DidYouMeanCandidates(ctx, viewerID, false, "dym-xxx")
	if err != nil {
		t.Fatalf("DidYouMeanCandidates (non-admin): %v", err)
	}
	assertStrings(t, got, "dym-act")

	admin, err := ls.DidYouMeanCandidates(ctx, viewerID, true, "dym-xxx")
	if err != nil {
		t.Fatalf("DidYouMeanCandidates (admin): %v", err)
	}
	assertStrings(t, admin, "dym-act")
}

// An empty query returns an empty candidate set, never the whole corpus.
func TestDidYouMeanCandidates_EmptyQueryReturnsNoCandidates(t *testing.T) {
	ls, _, viewerID, _ := newSuggestEnv(t)
	ctx := context.Background()

	mustCreate(t, ls, "ab", viewerID, "", "", "public")

	got, err := ls.DidYouMeanCandidates(ctx, viewerID, false, "")
	if err != nil {
		t.Fatalf("DidYouMeanCandidates(\"\"): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("DidYouMeanCandidates(\"\") = %v, want empty non-nil slice", got)
	}
}
