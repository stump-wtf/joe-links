// Tests for the destination health checker, named after the SPEC-0020 REQ
// "Destination Health Checking" scenarios so the spec↔test mapping is
// auditable. Probes target local httptest servers, so functional tests run
// with AllowPrivate=true (the loopback classifier is exercised separately in
// guard_test.go).
//
// Governing: SPEC-0020 REQ "Destination Health Checking", ADR-0020 (c)/(d)
package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// checkerEnv wires a real store over in-memory SQLite plus a Checker whose
// per-host spacing is shrunk so multi-probe cycles stay fast.
type checkerEnv struct {
	db     *sqlx.DB
	links  *store.LinkStore
	userID string
}

func newCheckerEnv(t *testing.T) *checkerEnv {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	u, err := us.Upsert(context.Background(), "test", "sub-health", "health@example.com", "Health", "user")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return &checkerEnv{db: db, links: ls, userID: u.ID}
}

// newChecker builds a Checker against the env with test-friendly politeness.
func (e *checkerEnv) newChecker(cfg Config) *Checker {
	c := New(e.links, cfg)
	c.hostSpacing = time.Millisecond
	return c
}

func (e *checkerEnv) createLink(t *testing.T, slug, url string) *store.Link {
	t.Helper()
	link, err := e.links.CreateFull(context.Background(), slug, url, e.userID, "", "", "public", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("create link %s: %v", slug, err)
	}
	return link
}

// makeDue backdates next_check_at so the link is due again this cycle.
func (e *checkerEnv) makeDue(t *testing.T, linkID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := e.db.Exec(e.db.Rebind(`UPDATE link_health SET next_check_at = ? WHERE link_id = ?`), past, linkID); err != nil {
		t.Fatalf("make due: %v", err)
	}
}

func (e *checkerEnv) health(t *testing.T, linkID string) *store.LinkHealth {
	t.Helper()
	h, err := e.links.GetHealth(context.Background(), linkID)
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	return h
}

// countingServer returns an httptest server that answers with handler and an
// atomic request counter.
func countingServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// allowPrivateCfg is the baseline test config: httptest servers listen on
// loopback, which the default policy refuses, so functional probe tests opt
// in exactly as a homelab operator would.
func allowPrivateCfg(interval time.Duration) Config {
	return Config{Enabled: true, Interval: interval, Timeout: 5 * time.Second, AllowPrivate: true}
}

// Scenario: Healthy Destination
// WHEN the checker probes a link whose destination answers HEAD with 200
// THEN the link_health row records the status and time with
// consecutive_failures = 0, and the next check is scheduled one interval out.
func TestHealthChecker_HealthyDestination(t *testing.T) {
	env := newCheckerEnv(t)
	srv, count := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	link := env.createLink(t, "healthy", srv.URL)

	interval := time.Hour
	c := env.newChecker(allowPrivateCfg(interval))
	before := time.Now().UTC()
	c.runCycle(context.Background())

	if got := count.Load(); got != 1 {
		t.Fatalf("probe count = %d, want 1 (no retry within the cycle)", got)
	}
	h := env.health(t, link.ID)
	if h == nil {
		t.Fatal("no link_health row recorded")
	}
	if h.LastStatus == nil || *h.LastStatus != http.StatusOK {
		t.Errorf("last_status = %v, want 200", h.LastStatus)
	}
	if h.LastCheckedAt == nil || h.LastCheckedAt.Before(before.Add(-time.Second)) {
		t.Errorf("last_checked_at = %v, want ~now", h.LastCheckedAt)
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", h.ConsecutiveFailures)
	}
	if h.Skipped {
		t.Error("skipped = true, want false for a completed probe")
	}
	if h.NextCheckAt == nil {
		t.Fatal("next_check_at not scheduled")
	}
	gap := h.NextCheckAt.Sub(*h.LastCheckedAt)
	if gap < interval-time.Minute || gap > interval+time.Minute {
		t.Errorf("next check gap = %v, want one interval (%v)", gap, interval)
	}
	if got := store.DeriveHealth(link, h, time.Now().UTC()).Status; got != store.HealthOK {
		t.Errorf("derived health = %q, want %q", got, store.HealthOK)
	}
}

// Scenario: HEAD Rejected, GET Fallback
// WHEN a destination answers HEAD with 405 Method Not Allowed but GET with
// 200 THEN the probe is recorded as a success.
func TestHealthChecker_HEADRejectedGETFallback(t *testing.T) {
	env := newCheckerEnv(t)
	var sawGet atomic.Bool
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		sawGet.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	link := env.createLink(t, "head-hostile", srv.URL)

	c := env.newChecker(allowPrivateCfg(time.Hour))
	c.runCycle(context.Background())

	if !sawGet.Load() {
		t.Fatal("GET fallback was never issued after the 405 HEAD")
	}
	h := env.health(t, link.ID)
	if h == nil {
		t.Fatal("no link_health row recorded")
	}
	if h.LastStatus == nil || *h.LastStatus != http.StatusOK {
		t.Errorf("last_status = %v, want 200 from the GET fallback", h.LastStatus)
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0 (success)", h.ConsecutiveFailures)
	}
}

// Scenario: Failures Back Off and Eventually Mark Broken
// WHEN a destination fails three consecutive checks THEN the link is
// reported broken, with each next check scheduled at exponentially
// increasing gaps (interval × 2^(n−1), capped at 7 × interval) rather than
// retried within the same cycle.
func TestHealthChecker_FailuresBackOffAndEventuallyMarkBroken(t *testing.T) {
	env := newCheckerEnv(t)
	srv, count := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	link := env.createLink(t, "dead", srv.URL)

	interval := time.Hour
	c := env.newChecker(allowPrivateCfg(interval))

	wantGaps := []time.Duration{interval, 2 * interval, 4 * interval} // 2^(n−1) × interval
	for i, wantGap := range wantGaps {
		c.runCycle(context.Background())
		if got := count.Load(); got != int64(i+1) {
			t.Fatalf("cycle %d: cumulative probes = %d, want %d (one per cycle, no same-cycle retry)", i+1, got, i+1)
		}
		h := env.health(t, link.ID)
		if h == nil {
			t.Fatalf("cycle %d: no link_health row", i+1)
		}
		if h.ConsecutiveFailures != i+1 {
			t.Fatalf("cycle %d: consecutive_failures = %d, want %d", i+1, h.ConsecutiveFailures, i+1)
		}
		gap := h.NextCheckAt.Sub(*h.LastCheckedAt)
		if gap < wantGap-time.Minute || gap > wantGap+time.Minute {
			t.Errorf("cycle %d: backoff gap = %v, want %v", i+1, gap, wantGap)
		}
		env.makeDue(t, link.ID)
	}

	h := env.health(t, link.ID)
	if got := store.DeriveHealth(link, h, time.Now().UTC()).Status; got != store.HealthBroken {
		t.Errorf("derived health after 3 failures = %q, want %q", got, store.HealthBroken)
	}
	if h.LastStatus == nil || *h.LastStatus != http.StatusInternalServerError {
		t.Errorf("last_status = %v, want 500", h.LastStatus)
	}

	// The cap: 4 more failures would compute 8×, 16×, … — clamped at 7×.
	for i := 0; i < 4; i++ {
		env.makeDue(t, link.ID)
		c.runCycle(context.Background())
	}
	h = env.health(t, link.ID)
	gap := h.NextCheckAt.Sub(*h.LastCheckedAt)
	if want := 7 * interval; gap < want-time.Minute || gap > want+time.Minute {
		t.Errorf("backoff gap after %d failures = %v, want cap %v", h.ConsecutiveFailures, gap, want)
	}
}

// Scenario: 429 Is Not a Failure
// WHEN a destination answers 429 with Retry-After: 172800 THEN
// consecutive_failures does not change (neither increment nor reset) and the
// next check is at least 48 hours out.
func TestHealthChecker_429IsNotAFailure(t *testing.T) {
	env := newCheckerEnv(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "172800")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	link := env.createLink(t, "rate-limited", srv.URL)

	// Pre-existing failures prove "unchanged": a 429 must neither increment
	// (not a strike) nor reset (not a success) the counter.
	ctx := context.Background()
	status := http.StatusBadGateway
	for i := 0; i < 2; i++ {
		if _, err := env.links.RecordHealthFailure(ctx, link.ID, &status, "bad gateway", time.Now().UTC(), time.Hour); err != nil {
			t.Fatalf("seed failure: %v", err)
		}
	}
	env.makeDue(t, link.ID)

	c := env.newChecker(allowPrivateCfg(time.Hour))
	c.runCycle(ctx)

	h := env.health(t, link.ID)
	if h.ConsecutiveFailures != 2 {
		t.Errorf("consecutive_failures = %d, want unchanged 2", h.ConsecutiveFailures)
	}
	if h.LastStatus == nil || *h.LastStatus != http.StatusTooManyRequests {
		t.Errorf("last_status = %v, want 429", h.LastStatus)
	}
	if h.NextCheckAt == nil {
		t.Fatal("next_check_at not scheduled")
	}
	if gap := h.NextCheckAt.Sub(*h.LastCheckedAt); gap < 48*time.Hour-time.Minute {
		t.Errorf("next check gap = %v, want >= 48h (Retry-After honored)", gap)
	}
}

// The interval floor of the same scenario: a Retry-After smaller than one
// interval still pushes the next check out by at least one full interval.
func TestHealthChecker_429SmallRetryAfterStillDefersOneInterval(t *testing.T) {
	env := newCheckerEnv(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	link := env.createLink(t, "rate-limited-small", srv.URL)

	interval := 2 * time.Hour
	c := env.newChecker(allowPrivateCfg(interval))
	c.runCycle(context.Background())

	h := env.health(t, link.ID)
	if gap := h.NextCheckAt.Sub(*h.LastCheckedAt); gap < interval-time.Minute {
		t.Errorf("next check gap = %v, want >= one full interval (%v)", gap, interval)
	}
}

// Scenario: Opt-Out Honored
// WHEN an owner sets the health-check opt-out on a link THEN the checker
// does not probe that destination and the frozen link_health row is no
// longer surfaced: derived health reports "unchecked".
func TestHealthChecker_OptOutHonored(t *testing.T) {
	env := newCheckerEnv(t)
	srv, count := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	link := env.createLink(t, "opted-out", srv.URL)

	// A prior successful check leaves a frozen row behind the opt-out.
	ctx := context.Background()
	if _, err := env.links.RecordHealthSuccess(ctx, link.ID, http.StatusOK, time.Now().UTC(), time.Hour); err != nil {
		t.Fatalf("seed health row: %v", err)
	}
	env.makeDue(t, link.ID)
	optedOut, err := env.links.SetHealthChecksDisabled(ctx, link.ID, true)
	if err != nil {
		t.Fatalf("opt out: %v", err)
	}

	c := env.newChecker(allowPrivateCfg(time.Hour))
	c.runCycle(ctx)

	if got := count.Load(); got != 0 {
		t.Fatalf("probe count = %d, want 0 — opted-out links must never be fetched", got)
	}
	// The frozen row still exists but is not surfaced: unchecked, null details.
	h := env.health(t, link.ID)
	if h == nil {
		t.Fatal("frozen link_health row should remain in place")
	}
	view := store.DeriveHealth(optedOut, h, time.Now().UTC())
	if view.Status != store.HealthUnchecked {
		t.Errorf("surfaced status = %q, want %q (surfacing rule)", view.Status, store.HealthUnchecked)
	}
	if view.LastCheckedAt != nil || view.LastStatus != nil {
		t.Errorf("surfaced details = (%v, %v), want nulls", view.LastCheckedAt, view.LastStatus)
	}
}

// Scenario: Checker Disabled by Config
// WHEN the server runs with JOE_HEALTH_CHECKS_ENABLED=false THEN no probes
// are issued and no link_health rows are written.
func TestHealthChecker_CheckerDisabledByConfig(t *testing.T) {
	env := newCheckerEnv(t)
	srv, count := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.createLink(t, "unprobed", srv.URL)

	cfg := allowPrivateCfg(time.Hour)
	cfg.Enabled = false
	c := env.newChecker(cfg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(context.Background()) // must return immediately when disabled
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return immediately with the checker disabled")
	}

	if got := count.Load(); got != 0 {
		t.Errorf("probe count = %d, want 0", got)
	}
	var rows int
	if err := env.db.Get(&rows, `SELECT COUNT(*) FROM link_health`); err != nil {
		t.Fatalf("count link_health: %v", err)
	}
	if rows != 0 {
		t.Errorf("link_health rows = %d, want 0", rows)
	}
}

// Eligibility: variable links (SPEC-0009 URL templates) are never fetched —
// their destination is a template, not a URL.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Eligibility
func TestHealthChecker_VariableLinksAreSkippedEntirely(t *testing.T) {
	env := newCheckerEnv(t)
	env.createLink(t, "jira", "https://jira.example.com/browse/$ticket")

	c := env.newChecker(allowPrivateCfg(time.Hour))
	c.runCycle(context.Background())

	var rows int
	if err := env.db.Get(&rows, `SELECT COUNT(*) FROM link_health`); err != nil {
		t.Fatalf("count link_health: %v", err)
	}
	if rows != 0 {
		t.Errorf("link_health rows = %d, want 0 — no probe and no row for a variable link", rows)
	}
}

// Probe semantics: the identifying User-Agent names the checker and version.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Probe semantics
func TestHealthChecker_IdentifyingUserAgent(t *testing.T) {
	env := newCheckerEnv(t)
	var ua atomic.Value
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	})
	env.createLink(t, "attributed", srv.URL)

	c := env.newChecker(allowPrivateCfg(time.Hour))
	c.runCycle(context.Background())

	got, _ := ua.Load().(string)
	if !strings.HasPrefix(got, "joe-links-health/") {
		t.Errorf("User-Agent = %q, want joe-links-health/<version>", got)
	}
}

// Politeness: the wake-up cadence is a fraction of the check interval (capped,
// floored) so a healthy link whose next_check_at lands seconds after a cycle
// is picked up within minutes — once per interval, not once per two intervals.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Politeness
func TestPollInterval(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{24 * time.Hour, 5 * time.Minute},   // default interval → 5m cap
		{time.Hour, 5 * time.Minute},        // config floor → 5m cap (7.5m/8 > 5m)
		{16 * time.Minute, 2 * time.Minute}, // below the cap → interval/8
		{time.Second, time.Second},          // tiny test interval → 1s floor
	}
	for _, tc := range cases {
		if got := pollInterval(tc.interval); got != tc.want {
			t.Errorf("pollInterval(%v) = %v, want %v", tc.interval, got, tc.want)
		}
	}
	for _, tc := range cases {
		if got := pollInterval(tc.interval); got > tc.interval && tc.interval >= time.Second {
			t.Errorf("pollInterval(%v) = %v exceeds the interval", tc.interval, got)
		}
	}
}
