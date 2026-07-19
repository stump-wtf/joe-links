// Tests for the click-retention pruner, named after the SPEC-0021 REQ "Click
// Retention" scenarios so the spec↔test mapping is auditable:
//   - "Default is no deletion"
//   - "Opt-in pruning"
//   - "Batched deletes"
//   - "Startup surfacing"
//
// ("Labeled totals under retention" is a stats-page concern, pinned in
// internal/handler; the config-validation constraints live in
// internal/config.)
//
// Governing: SPEC-0021 REQ "Click Retention", ADR-0021 (e)
package retention

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/joestump/joe-links/internal/metrics"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newPrunerEnv builds a ClickStore over a migrated in-memory database with
// one seeded link to attach clicks to (link_clicks.link_id carries an
// enforced foreign key).
func newPrunerEnv(t *testing.T) (*sqlx.DB, *store.ClickStore, string) {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)

	ctx := context.Background()
	owner, err := us.Upsert(ctx, "test", "prune-owner", "prune-owner@example.com", "Prune Owner", "user")
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	link, err := ls.Create(ctx, "prune-link", "https://example.com/prune", owner.ID, "Prune", "", "private")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	return db, store.NewClickStore(db), link.ID
}

// seedClickAgeDays inserts a click row clicked_at the given number of days ago.
func seedClickAgeDays(t *testing.T, db *sqlx.DB, linkID string, daysAgo int) {
	t.Helper()
	ts := time.Now().UTC().AddDate(0, 0, -daysAgo)
	_, err := db.ExecContext(context.Background(), db.Rebind(`
		INSERT INTO link_clicks (id, link_id, user_id, ip_hash, user_agent, referrer, clicked_at)
		VALUES (?, ?, NULL, 'h', '', '', ?)
	`), uuid.New().String(), linkID, ts)
	if err != nil {
		t.Fatalf("seed click %d days ago: %v", daysAgo, err)
	}
}

// countClicks returns the number of link_clicks rows.
func countClicks(t *testing.T, db *sqlx.DB) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM link_clicks`); err != nil {
		t.Fatalf("count clicks: %v", err)
	}
	return n
}

// captureLog redirects the standard logger into a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// Scenario: Default is no deletion — with JOE_CLICK_RETENTION unset (Days 0)
// the pruner never deletes a click row: Run returns immediately without
// touching the database.
// Governing: SPEC-0021 REQ "Click Retention"
func TestRetention_DefaultIsNoDeletion(t *testing.T) {
	db, cs, linkID := newPrunerEnv(t)
	for _, daysAgo := range []int{1, 400, 4000} {
		seedClickAgeDays(t, db, linkID, daysAgo)
	}

	p := New(cs, Config{Days: 0})
	// Run must return immediately when retention is disabled — an undeadlined
	// context proves it does not sit in the ticker loop.
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return immediately with retention disabled")
	}

	if got := countClicks(t, db); got != 3 {
		t.Errorf("click rows after disabled Run = %d, want 3 (no deletion, ever)", got)
	}
}

// Scenario: Opt-in pruning — with JOE_CLICK_RETENTION=365 a pruner run
// deletes the 400-day-old rows, leaves the 300-day-old rows, logs the count,
// and increments joelinks_clicks_pruned_total.
// Governing: SPEC-0021 REQ "Click Retention"
func TestRetention_OptInPruning(t *testing.T) {
	db, cs, linkID := newPrunerEnv(t)
	seedClickAgeDays(t, db, linkID, 400)
	seedClickAgeDays(t, db, linkID, 400)
	seedClickAgeDays(t, db, linkID, 300)

	buf := captureLog(t)
	before := promtestutil.ToFloat64(metrics.ClicksPrunedTotal)

	p := New(cs, Config{Days: 365})
	p.runOnce(context.Background())

	if got := countClicks(t, db); got != 1 {
		t.Errorf("click rows after prune = %d, want 1 (only the 300-day-old row survives)", got)
	}
	if !strings.Contains(buf.String(), "pruned 2 click rows") {
		t.Errorf("prune run must log the number of rows pruned; log=%q", buf.String())
	}
	if got := promtestutil.ToFloat64(metrics.ClicksPrunedTotal) - before; got != 2 {
		t.Errorf("joelinks_clicks_pruned_total increment = %v, want 2", got)
	}
}

// Scenario: Batched deletes — when more rows than one batch are older than
// the horizon at prune time, the pruner deletes them across at least three
// bounded batches within the run (batch size shrunk from the production
// 10,000 so the multi-batch path runs against 25 rows instead of 25,000).
// Governing: SPEC-0021 REQ "Click Retention"
func TestRetention_BatchedDeletes(t *testing.T) {
	db, cs, linkID := newPrunerEnv(t)
	for i := 0; i < 25; i++ {
		seedClickAgeDays(t, db, linkID, 400+i)
	}
	seedClickAgeDays(t, db, linkID, 30) // inside the horizon: must survive

	buf := captureLog(t)
	p := New(cs, Config{Days: 365})
	p.batchSize = 10 // 25 victims / 10 per statement = 3 bounded batches
	p.runOnce(context.Background())

	if got := countClicks(t, db); got != 1 {
		t.Errorf("click rows after batched prune = %d, want 1 (all 25 aged rows deleted across batches)", got)
	}
	if !strings.Contains(buf.String(), "pruned 25 click rows") {
		t.Errorf("prune run must log the total across all batches; log=%q", buf.String())
	}
}

// Scenario: Startup surfacing — when the server starts with
// JOE_CLICK_RETENTION=365, a log line states that click retention is enabled
// with a 365-day horizon (pruned rows are unrecoverable, so activation must
// be visible).
// Governing: SPEC-0021 REQ "Click Retention"
func TestRetention_StartupSurfacing(t *testing.T) {
	_, cs, _ := newPrunerEnv(t)

	buf := captureLog(t)
	// A pre-cancelled context makes Run synchronous: it logs the startup
	// line, performs the (empty) startup prune, and exits at the first tick
	// select — exactly the serve-shutdown path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := New(cs, Config{Days: 365})
	p.Run(ctx)

	logged := buf.String()
	if !strings.Contains(logged, "click retention enabled") || !strings.Contains(logged, fmt.Sprintf("%d-day horizon", 365)) {
		t.Errorf("startup must surface active retention and its horizon; log=%q", logged)
	}
}
