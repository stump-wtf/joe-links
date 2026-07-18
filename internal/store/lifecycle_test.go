// Store-layer tests for the link lifecycle data model: the expires_at /
// archived_at columns added by migration 00016, the derived lifecycle state,
// and the expiration set/clear/validate rules every write surface shares.
// Tests are named after the SPEC-0020 REQ "Link Expiration" scenarios so the
// spec↔test mapping is auditable.
//
// Governing: SPEC-0020 REQ "Link Expiration", REQ "Archive State", ADR-0020
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newLifecycleEnv is newTestEnv plus the raw DB handle, so tests can backdate
// expires_at directly (the store itself refuses to write new past values).
func newLifecycleEnv(t *testing.T) (*store.LinkStore, *sqlx.DB, string) {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)

	u, err := us.Upsert(context.Background(), "test", "sub-lifecycle", "lifecycle@example.com", "Lifecycle", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return ls, db, u.ID
}

// backdateExpiry forces expires_at into the past via raw SQL, simulating a
// link whose expiration has since passed.
func backdateExpiry(t *testing.T, db *sqlx.DB, linkID string, expiresAt time.Time) {
	t.Helper()
	if _, err := db.Exec(db.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`),
		expiresAt.UTC().Truncate(time.Second), linkID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
}

// Scenario: Link Created with Expiration
// WHEN a user creates a link with expires_at set to a future timestamp
// THEN the link is persisted with that expires_at (derived state active until then).
func TestLifecycle_LinkCreatedWithExpiration(t *testing.T) {
	ls, _, userID := newLifecycleEnv(t)
	ctx := context.Background()

	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link, err := ls.CreateFull(ctx, "expires-soon", "https://example.com", userID, "", "", "public", &future, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateFull with expiration: %v", err)
	}
	if link.ExpiresAt == nil || !link.ExpiresAt.Equal(future) {
		t.Fatalf("stored expires_at = %v, want %v", link.ExpiresAt, future)
	}

	// Derived at read time: active before the deadline, expired after it —
	// no background job, no stored status value.
	now := time.Now().UTC()
	if got := link.LifecycleState(now); got != store.LifecycleActive {
		t.Errorf("LifecycleState(now) = %q, want %q", got, store.LifecycleActive)
	}
	if got := link.LifecycleState(future.Add(time.Second)); got != store.LifecycleExpired {
		t.Errorf("LifecycleState(after expiry) = %q, want %q", got, store.LifecycleExpired)
	}

	// Omitting the expiration yields NULL — the default for new links.
	plain, err := ls.CreateFull(ctx, "never-expires", "https://example.com/2", userID, "", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateFull without expiration: %v", err)
	}
	if plain.ExpiresAt != nil {
		t.Errorf("expires_at without input = %v, want nil", plain.ExpiresAt)
	}
}

// Scenario: Past Expiration Rejected
// WHEN a create or update sets expires_at to a past timestamp that differs
// from the stored value THEN a validation error is returned and nothing is persisted.
func TestLifecycle_PastExpirationRejected(t *testing.T) {
	ls, _, userID := newLifecycleEnv(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Hour)
	if _, err := ls.CreateFull(ctx, "born-dead", "https://example.com", userID, "", "", "public", &past, nil, nil, ""); !errors.Is(err, store.ErrExpiresAtInPast) {
		t.Fatalf("CreateFull past expiry = %v, want ErrExpiresAtInPast", err)
	}
	if _, err := ls.GetBySlug(ctx, "born-dead"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("link exists after rejected create, GetBySlug err = %v", err)
	}

	link, err := ls.CreateFull(ctx, "stays-alive", "https://example.com", userID, "", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if _, err := ls.Update(ctx, link.ID, link.URL, "", "", "public", &past); !errors.Is(err, store.ErrExpiresAtInPast) {
		t.Fatalf("Update past expiry = %v, want ErrExpiresAtInPast", err)
	}
	got, err := ls.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at after rejected update = %v, want nil", got.ExpiresAt)
	}
}

// Scenario: Expired Link Stays Editable
// WHEN an owner edits an expired link round-tripping the stored (past)
// expires_at unchanged THEN the update succeeds and expires_at is untouched.
func TestLifecycle_ExpiredLinkStaysEditable(t *testing.T) {
	ls, db, userID := newLifecycleEnv(t)
	ctx := context.Background()

	link, err := ls.CreateFull(ctx, "expired-link", "https://example.com", userID, "Old Title", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	past := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	backdateExpiry(t, db, link.ID, past)

	stored, err := ls.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !stored.IsExpired(time.Now().UTC()) {
		t.Fatalf("precondition failed: link not expired, expires_at=%v", stored.ExpiresAt)
	}

	// Round-trip the stored past value, editing only the title — exactly what
	// the edit form and a full-resource PUT send.
	updated, err := ls.Update(ctx, link.ID, stored.URL, "New Title", "", "public", stored.ExpiresAt)
	if err != nil {
		t.Fatalf("Update round-tripping past expires_at: %v", err)
	}
	if updated.Title != "New Title" {
		t.Errorf("title = %q, want %q", updated.Title, "New Title")
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(past) {
		t.Errorf("expires_at = %v, want unchanged %v", updated.ExpiresAt, past)
	}
}

// Scenario: Expiration Cleared on Edit
// WHEN an owner clears the expiration THEN stored expires_at becomes NULL and
// the link no longer expires.
func TestLifecycle_ExpirationClearedOnEdit(t *testing.T) {
	ls, db, userID := newLifecycleEnv(t)
	ctx := context.Background()

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	link, err := ls.CreateFull(ctx, "clear-me", "https://example.com", userID, "", "", "public", &future, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}

	updated, err := ls.Update(ctx, link.ID, link.URL, "", "", "public", nil)
	if err != nil {
		t.Fatalf("Update clearing expires_at: %v", err)
	}
	if updated.ExpiresAt != nil {
		t.Errorf("expires_at after clear = %v, want nil", updated.ExpiresAt)
	}
	if got := updated.LifecycleState(future.Add(time.Hour)); got != store.LifecycleActive {
		t.Errorf("LifecycleState after clear = %q, want %q", got, store.LifecycleActive)
	}

	// Clearing also works on an already-expired link (the web form's empty
	// input on an expired link's edit form).
	backdateExpiry(t, db, link.ID, time.Now().UTC().Add(-time.Hour))
	cleared, err := ls.Update(ctx, link.ID, link.URL, "", "", "public", nil)
	if err != nil {
		t.Fatalf("Update clearing past expires_at: %v", err)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("expires_at after clearing expired link = %v, want nil", cleared.ExpiresAt)
	}
}

// Archive-state schema semantics (SPEC-0020 REQ "Archive State", scenario
// "Archived Beats Expired in Derived State"): the archived_at column exists,
// is independent of expires_at, and wins in the derived state. The archive
// toggle endpoints land in story #273 — this pins the column + derivation.
func TestLifecycle_ArchivedBeatsExpiredInDerivedState(t *testing.T) {
	ls, db, userID := newLifecycleEnv(t)
	ctx := context.Background()

	link, err := ls.CreateFull(ctx, "both-states", "https://example.com", userID, "", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := db.Exec(db.Rebind(`UPDATE links SET archived_at = ?, expires_at = ? WHERE id = ?`),
		now.Add(-time.Hour), now.Add(-2*time.Hour), link.ID); err != nil {
		t.Fatalf("set lifecycle columns: %v", err)
	}

	got, err := ls.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.IsArchived() || !got.IsExpired(now) {
		t.Fatalf("precondition failed: archived=%v expired=%v", got.IsArchived(), got.IsExpired(now))
	}
	if state := got.LifecycleState(now); state != store.LifecycleArchived {
		t.Errorf("LifecycleState = %q, want %q (archived wins)", state, store.LifecycleArchived)
	}
}

// ValidateExpiresAt is the one validation rule all write surfaces share; pin
// its edge cases directly.
// Governing: SPEC-0020 REQ "Link Expiration"
func TestLifecycle_ValidateExpiresAt(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	cases := []struct {
		name    string
		newVal  *time.Time
		stored  *time.Time
		wantErr bool
	}{
		{"nil is always valid", nil, nil, false},
		{"clearing a past value is valid", nil, &past, false},
		{"future value is valid", &future, nil, false},
		{"new past value rejected", &past, nil, true},
		{"new past value rejected on update", &past, &future, true},
		{"round-tripped past value accepted", &past, &past, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.ValidateExpiresAt(tc.newVal, tc.stored, now)
			if tc.wantErr && !errors.Is(err, store.ErrExpiresAtInPast) {
				t.Errorf("ValidateExpiresAt = %v, want ErrExpiresAtInPast", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateExpiresAt = %v, want nil", err)
			}
		})
	}
}
