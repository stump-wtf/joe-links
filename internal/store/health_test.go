// Store-layer tests for SPEC-0020 destination health and staleness: the
// link_health derivation and surfacing rule, checker eligibility, the admin
// broken-links report query, the staleness views, and the lifecycle exclusion
// of expired/archived links from public browsing (the PR #290 debt — all
// public-surface call sites carry the lifecycle predicate). Tests are named
// after the spec scenarios so the spec↔test mapping is auditable.
//
// Governing: SPEC-0020 REQ "Destination Health Checking", REQ "Health Badges
// and Admin Report", REQ "Staleness Views", ADR-0020
package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

type healthEnv struct {
	ls     *store.LinkStore
	db     *sqlx.DB
	userID string
}

func newHealthEnv(t *testing.T) *healthEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	u, err := us.Upsert(context.Background(), "test", "sub-health-store", "health-store@example.com", "Health Store", "user")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return &healthEnv{ls: ls, db: db, userID: u.ID}
}

func (e *healthEnv) createLink(t *testing.T, slug, visibility string) *store.Link {
	t.Helper()
	link, err := e.ls.CreateFull(context.Background(), slug, "https://example.com/"+slug, e.userID, "", "", visibility, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("create link %s: %v", slug, err)
	}
	return link
}

// seedFailures records n consecutive failed probes for a link.
func (e *healthEnv) seedFailures(t *testing.T, linkID string, n int) {
	t.Helper()
	status := 503
	for i := 0; i < n; i++ {
		if _, err := e.ls.RecordHealthFailure(context.Background(), linkID, &status, "unavailable", time.Now().UTC(), time.Hour); err != nil {
			t.Fatalf("seed failure: %v", err)
		}
	}
}

// backdateCreated forces created_at into the past for staleness tests.
func (e *healthEnv) backdateCreated(t *testing.T, linkID string, createdAt time.Time) {
	t.Helper()
	if _, err := e.db.Exec(e.db.Rebind(`UPDATE links SET created_at = ? WHERE id = ?`), createdAt.UTC(), linkID); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}
}

// insertClick plants a raw click row at a given time.
func (e *healthEnv) insertClick(t *testing.T, linkID string, clickedAt time.Time) {
	t.Helper()
	if _, err := e.db.Exec(e.db.Rebind(`
		INSERT INTO link_clicks (id, link_id, user_id, ip_hash, user_agent, referrer, clicked_at)
		VALUES (?, ?, NULL, 'test-hash', '', '', ?)
	`), uuid.NewString(), linkID, clickedAt.UTC()); err != nil {
		t.Fatalf("insert click: %v", err)
	}
}

func (e *healthEnv) archive(t *testing.T, linkID string) {
	t.Helper()
	if _, err := e.ls.SetArchived(context.Background(), linkID, true); err != nil {
		t.Fatalf("archive: %v", err)
	}
}

func (e *healthEnv) expire(t *testing.T, linkID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := e.db.Exec(e.db.Rebind(`UPDATE links SET expires_at = ? WHERE id = ?`), past, linkID); err != nil {
		t.Fatalf("expire link: %v", err)
	}
}

// The surfacing rule: health state is surfaced only while the link is
// eligible for checking — opted-out, archived, and expired links report
// "unchecked" with null details even when a frozen link_health row exists.
// Governing: SPEC-0020 REQ "Destination Health Checking" — State, surfacing rule
func TestDeriveHealth_SurfacingRule(t *testing.T) {
	now := time.Now().UTC()
	checked := now.Add(-time.Hour)
	status := 503
	row := &store.LinkHealth{LastCheckedAt: &checked, LastStatus: &status, ConsecutiveFailures: 3}

	active := &store.Link{}
	if got := store.DeriveHealth(active, row, now); got.Status != store.HealthBroken {
		t.Errorf("active link with 3 failures = %q, want broken", got.Status)
	}
	if got := store.DeriveHealth(active, nil, now); got.Status != store.HealthUnchecked {
		t.Errorf("no row = %q, want unchecked", got.Status)
	}
	okRow := &store.LinkHealth{LastCheckedAt: &checked, LastStatus: &status, ConsecutiveFailures: 2}
	if got := store.DeriveHealth(active, okRow, now); got.Status != store.HealthOK {
		t.Errorf("2 failures = %q, want ok (broken needs >= 3)", got.Status)
	}
	skippedRow := &store.LinkHealth{LastCheckedAt: &checked, Skipped: true}
	if got := store.DeriveHealth(active, skippedRow, now); got.Status != store.HealthSkipped {
		t.Errorf("skipped row = %q, want skipped", got.Status)
	}

	frozenCases := map[string]*store.Link{
		"opted out": {HealthChecksDisabled: true},
		"archived":  {ArchivedAt: &checked},
		"expired":   {ExpiresAt: &checked},
	}
	for name, link := range frozenCases {
		got := store.DeriveHealth(link, row, now)
		if got.Status != store.HealthUnchecked {
			t.Errorf("%s link = %q, want unchecked (frozen row not surfaced)", name, got.Status)
		}
		if got.LastCheckedAt != nil || got.LastStatus != nil {
			t.Errorf("%s link details = (%v, %v), want nulls", name, got.LastCheckedAt, got.LastStatus)
		}
	}
}

// Eligibility: each cycle considers only due links and skips archived,
// expired, opted-out, and variable links — no fetch may be issued for any of
// them.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Eligibility
func TestListDueForHealthCheck_EligibilityFilter(t *testing.T) {
	env := newHealthEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()

	due := env.createLink(t, "due", "public")
	archived := env.createLink(t, "was-archived", "public")
	env.archive(t, archived.ID)
	expired := env.createLink(t, "was-expired", "public")
	env.expire(t, expired.ID)
	optedOut := env.createLink(t, "opted-out", "public")
	if _, err := env.ls.SetHealthChecksDisabled(ctx, optedOut.ID, true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	if _, err := env.ls.CreateFull(ctx, "tmpl", "https://example.com/$id", env.userID, "", "", "public", nil, nil, nil, ""); err != nil {
		t.Fatalf("create variable link: %v", err)
	}
	// A link checked recently is not due until next_check_at passes.
	fresh := env.createLink(t, "fresh", "public")
	if _, err := env.ls.RecordHealthSuccess(ctx, fresh.ID, 200, now, time.Hour); err != nil {
		t.Fatalf("seed fresh check: %v", err)
	}

	got, err := env.ls.ListDueForHealthCheck(ctx, now)
	if err != nil {
		t.Fatalf("ListDueForHealthCheck: %v", err)
	}
	if len(got) != 1 || got[0].ID != due.ID {
		slugs := make([]string, len(got))
		for i, l := range got {
			slugs[i] = l.Slug
		}
		t.Errorf("due links = %v, want exactly [due]", slugs)
	}
}

// Scenario: Admin Report Lists Failing Links (store query half)
// WHEN two links are broken THEN both are listed with status, owner, and
// last-checked details, most failures first; healthy, opted-out, skipped,
// and never-checked links are not listed.
func TestListBrokenLinks_AdminReportListsFailingLinks(t *testing.T) {
	env := newHealthEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()

	worse := env.createLink(t, "worse", "public")
	env.seedFailures(t, worse.ID, 5)
	broken := env.createLink(t, "broken", "public")
	env.seedFailures(t, broken.ID, 3)

	healthy := env.createLink(t, "healthy", "public")
	if _, err := env.ls.RecordHealthSuccess(ctx, healthy.ID, 200, now, time.Hour); err != nil {
		t.Fatalf("seed healthy: %v", err)
	}
	almost := env.createLink(t, "almost", "public")
	env.seedFailures(t, almost.ID, 2)
	skipped := env.createLink(t, "skipped", "public")
	if _, err := env.ls.RecordHealthSkipped(ctx, skipped.ID, "policy", now, time.Hour); err != nil {
		t.Fatalf("seed skipped: %v", err)
	}
	optedOut := env.createLink(t, "opted-out-broken", "public")
	env.seedFailures(t, optedOut.ID, 4)
	if _, err := env.ls.SetHealthChecksDisabled(ctx, optedOut.ID, true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	archived := env.createLink(t, "archived-broken", "public")
	env.seedFailures(t, archived.ID, 4)
	env.archive(t, archived.ID)
	env.createLink(t, "never-checked", "public")

	rows, err := env.ls.ListBrokenLinks(ctx, now)
	if err != nil {
		t.Fatalf("ListBrokenLinks: %v", err)
	}
	if len(rows) != 2 {
		slugs := make([]string, len(rows))
		for i, r := range rows {
			slugs[i] = r.Slug
		}
		t.Fatalf("broken rows = %v, want exactly [worse broken] (most failures first)", slugs)
	}
	if rows[0].Slug != "worse" || rows[1].Slug != "broken" {
		t.Errorf("order = [%s %s], want [worse broken]", rows[0].Slug, rows[1].Slug)
	}
	if rows[0].ConsecutiveFailures != 5 {
		t.Errorf("worse failures = %d, want 5", rows[0].ConsecutiveFailures)
	}
	if rows[0].LastStatus == nil || *rows[0].LastStatus != 503 {
		t.Errorf("worse last_status = %v, want 503", rows[0].LastStatus)
	}
	if rows[0].LastCheckedAt == nil {
		t.Error("worse last_checked_at missing")
	}
	if rows[0].Owners == "" {
		t.Error("owner display names missing from the report row")
	}
}

// Scenario: Stale Filter
// WHEN an owner applies the "stale" filter and has a 6-month-old link whose
// last click was 4 months ago THEN that link appears; a link clicked
// yesterday does not.
func TestStaleness_StaleFilter(t *testing.T) {
	env := newHealthEnv(t)
	now := time.Now().UTC()

	stale := env.createLink(t, "stale", "public")
	env.backdateCreated(t, stale.ID, now.Add(-6*30*24*time.Hour))
	env.insertClick(t, stale.ID, now.Add(-4*30*24*time.Hour))

	active := env.createLink(t, "active", "public")
	env.backdateCreated(t, active.ID, now.Add(-6*30*24*time.Hour))
	env.insertClick(t, active.ID, now.Add(-24*time.Hour))

	// Archived links are out of scope: archiving is deliberate retirement.
	retired := env.createLink(t, "retired", "public")
	env.backdateCreated(t, retired.ID, now.Add(-6*30*24*time.Hour))
	env.archive(t, retired.ID)

	// Young links are not stale regardless of clicks.
	env.createLink(t, "young", "public")

	got, err := env.ls.ListStaleByOwner(context.Background(), env.userID, now)
	if err != nil {
		t.Fatalf("ListStaleByOwner: %v", err)
	}
	if len(got) != 1 || got[0].ID != stale.ID {
		slugs := make([]string, len(got))
		for i, l := range got {
			slugs[i] = l.Slug
		}
		t.Errorf("stale links = %v, want exactly [stale]", slugs)
	}
}

// Scenario: Never-Clicked Filter
// WHEN an owner applies the "never clicked" filter and has a 3-week-old link
// with zero clicks and a 2-day-old link with zero clicks THEN the 3-week-old
// link appears and the 2-day-old link does not (creation grace period).
func TestStaleness_NeverClickedFilter(t *testing.T) {
	env := newHealthEnv(t)
	now := time.Now().UTC()

	neverClicked := env.createLink(t, "never-clicked", "public")
	env.backdateCreated(t, neverClicked.ID, now.Add(-21*24*time.Hour))

	fresh := env.createLink(t, "brand-new", "public")
	env.backdateCreated(t, fresh.ID, now.Add(-2*24*time.Hour))

	clicked := env.createLink(t, "clicked-once", "public")
	env.backdateCreated(t, clicked.ID, now.Add(-21*24*time.Hour))
	env.insertClick(t, clicked.ID, now.Add(-20*24*time.Hour))

	got, err := env.ls.ListNeverClickedByOwner(context.Background(), env.userID, now)
	if err != nil {
		t.Fatalf("ListNeverClickedByOwner: %v", err)
	}
	if len(got) != 1 || got[0].ID != neverClicked.ID {
		slugs := make([]string, len(got))
		for i, l := range got {
			slugs[i] = l.Slug
		}
		t.Errorf("never-clicked links = %v, want exactly [never-clicked]", slugs)
	}
}

// Scenario: Staleness Respects Visibility Scope (store half — the by-owner
// queries return only the viewer's own links; the dashboard handler test
// covers the surface wiring)
func TestStaleness_StalenessRespectsVisibilityScope(t *testing.T) {
	env := newHealthEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()

	us := store.NewUserStore(env.db)
	other, err := us.Upsert(ctx, "test", "sub-other-owner", "other-owner@example.com", "Other", "user")
	if err != nil {
		t.Fatalf("seed other user: %v", err)
	}

	mine := env.createLink(t, "mine-stale", "public")
	env.backdateCreated(t, mine.ID, now.Add(-100*24*time.Hour))

	theirs, err := env.ls.CreateFull(ctx, "theirs-stale", "https://example.com/theirs", other.ID, "", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("create other link: %v", err)
	}
	env.backdateCreated(t, theirs.ID, now.Add(-100*24*time.Hour))

	got, err := env.ls.ListStaleByOwner(ctx, env.userID, now)
	if err != nil {
		t.Fatalf("ListStaleByOwner: %v", err)
	}
	for _, l := range got {
		if l.ID == theirs.ID {
			t.Fatalf("stale filter leaked another owner's link %q", l.Slug)
		}
	}
	if len(got) != 1 || got[0].ID != mine.ID {
		t.Errorf("got %d stale links, want exactly the viewer's own", len(got))
	}

	// The never-clicked variant scopes identically.
	gotNC, err := env.ls.ListNeverClickedByOwner(ctx, env.userID, now)
	if err != nil {
		t.Fatalf("ListNeverClickedByOwner: %v", err)
	}
	for _, l := range gotNC {
		if l.ID == theirs.ID {
			t.Fatalf("never-clicked filter leaked another owner's link %q", l.Slug)
		}
	}
}

// PR #290 debt, item 2: expired and archived links must not be enumerable on
// public surfaces — ListPublic (web public browser and MCP list_links
// filter=public) and ListPublicByOwner (profile pages) carry the lifecycle
// predicate for ALL callers.
// Governing: SPEC-0020 REQ "Health Badges and Admin Report" scenario "Public
// Browser Shows No Health Data"; Security "Resolution Ordering and Oracle Resistance"
func TestPublicBrowsing_ExcludesExpiredAndArchivedForAllCallers(t *testing.T) {
	env := newHealthEnv(t)
	ctx := context.Background()

	visible := env.createLink(t, "public-live", "public")
	expired := env.createLink(t, "public-expired", "public")
	env.expire(t, expired.ID)
	archived := env.createLink(t, "public-archived", "public")
	env.archive(t, archived.ID)

	assertOnlyLive := func(t *testing.T, label string, rows []*store.AdminLink, total int) {
		t.Helper()
		if total != 1 || len(rows) != 1 || rows[0].ID != visible.ID {
			slugs := make([]string, len(rows))
			for i, r := range rows {
				slugs[i] = r.Slug
			}
			t.Errorf("%s = %v (total %d), want exactly [public-live]", label, slugs, total)
		}
	}

	// Anonymous and authenticated callers alike: the exclusion is for ALL
	// callers, including the owner, so lifecycle states cannot be enumerated.
	for _, caller := range []struct {
		name string
		id   string
	}{{"anonymous", ""}, {"owner", env.userID}} {
		rows, total, err := env.ls.ListPublic(ctx, caller.id, "", 1, 50)
		if err != nil {
			t.Fatalf("ListPublic (%s): %v", caller.name, err)
		}
		assertOnlyLive(t, fmt.Sprintf("ListPublic (%s)", caller.name), rows, total)

		byOwner, totalOwner, err := env.ls.ListPublicByOwner(ctx, env.userID, caller.id, 1, 50)
		if err != nil {
			t.Fatalf("ListPublicByOwner (%s): %v", caller.name, err)
		}
		assertOnlyLive(t, fmt.Sprintf("ListPublicByOwner (%s)", caller.name), byOwner, totalOwner)
	}

	// The search path shares the predicate too.
	rows, total, err := env.ls.ListPublic(ctx, "", "public", 1, 50)
	if err != nil {
		t.Fatalf("ListPublic search: %v", err)
	}
	assertOnlyLive(t, "ListPublic search", rows, total)
}

// The backoff schedule is pure arithmetic shared by the checker: interval ×
// 2^(n−1), capped at 7 × interval.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Politeness
func TestHealthBackoff_Schedule(t *testing.T) {
	interval := time.Hour
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, interval},
		{1, interval},
		{2, 2 * interval},
		{3, 4 * interval},
		{4, 7 * interval}, // 8× clamps to the 7× cap
		{10, 7 * interval},
	}
	for _, tc := range cases {
		if got := store.HealthBackoff(interval, tc.failures); got != tc.want {
			t.Errorf("HealthBackoff(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}
