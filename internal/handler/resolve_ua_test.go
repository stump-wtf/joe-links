package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// Governing: SPEC-0016 REQ "Click Data Schema" — user-agent truncation must be
// rune-safe. The resolver used to byte-slice the UA at 512 before the store's
// 512-rune truncation; a multi-byte character straddling byte 512 was split
// into invalid UTF-8 (issue #205). The resolver now passes the UA through
// untouched and RecordClick alone enforces the cap.
func TestResolve_ClickUserAgent_MultibyteBoundarySurvives(t *testing.T) {
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	ks := store.NewKeywordStore(db)
	us := store.NewUserStore(db)

	u, err := us.Upsert(context.Background(), "test", "sub1", "test@example.com", "Test", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := ls.Create(context.Background(), "ua-link", "https://example.com", u.ID, "", "", ""); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	clickCh := make(chan store.ClickEvent, 1)
	rh := NewResolveHandler(ls, ks, owns, clickCh)
	r := chi.NewRouter()
	r.Get("/{slug}*", rh.Resolve)

	// 510 ASCII bytes followed by two 3-byte runes: the first "€" occupies
	// bytes 511-513, exactly straddling the old ua[:512] byte cut. Total is
	// 512 runes, so the store's rune cap must leave it fully intact.
	ua := strings.Repeat("a", 510) + "€€"
	req := httptest.NewRequest(http.MethodGet, "/ua-link", nil)
	req.Header.Set("User-Agent", ua)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}

	select {
	case ev := <-clickCh:
		if !utf8.ValidString(ev.UserAgent) {
			t.Errorf("click user-agent is invalid UTF-8: %q", ev.UserAgent)
		}
		if ev.UserAgent != ua {
			t.Errorf("user-agent mangled: got %d bytes, want %d bytes (multibyte char split at byte 512?)", len(ev.UserAgent), len(ua))
		}
	default:
		t.Fatal("no click event recorded")
	}
}
