// Tests for SPEC-0019 REQ "Case-Folded Keyword Matching" (issue #284): the
// keyword store folds the candidate before comparison, so lookup behaves the
// same on every driver — MySQL's case-insensitive default collation already
// folded, sqlite3 and PostgreSQL did not.
// Governing: SPEC-0019 REQ "Case-Folded Keyword Matching", ADR-0011, ADR-0027
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

func TestKeywordStore_GetByKeywordCaseFoldsLookup(t *testing.T) {
	db := testutil.NewTestDB(t)
	ks := store.NewKeywordStore(db)

	if _, err := ks.Create(context.Background(), "gh", "https://github.com/{slug}", "GitHub"); err != nil {
		t.Fatalf("Create keyword: %v", err)
	}

	for _, candidate := range []string{"gh", "GH", "Gh", "gH"} {
		kw, err := ks.GetByKeyword(context.Background(), candidate)
		if err != nil {
			t.Fatalf("GetByKeyword(%q): %v", candidate, err)
		}
		if kw.Keyword != "gh" {
			t.Errorf("GetByKeyword(%q).Keyword = %q, want %q", candidate, kw.Keyword, "gh")
		}
	}
}

func TestKeywordStore_GetByKeywordStillMissesOtherNames(t *testing.T) {
	db := testutil.NewTestDB(t)
	ks := store.NewKeywordStore(db)

	if _, err := ks.Create(context.Background(), "gh", "https://github.com/{slug}", ""); err != nil {
		t.Fatalf("Create keyword: %v", err)
	}

	for _, candidate := range []string{"ghh", "g-h", "GHX", ""} {
		if _, err := ks.GetByKeyword(context.Background(), candidate); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetByKeyword(%q) error = %v, want ErrNotFound", candidate, err)
		}
	}
}
