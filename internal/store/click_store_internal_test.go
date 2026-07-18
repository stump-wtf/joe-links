package store

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/joestump/joe-links/internal/testutil"
)

// Governing: SPEC-0016 REQ "Click Data Schema" — rune-aware length truncation.
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 512); got != "hello" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncateRunes(strings.Repeat("a", 600), 512); len([]rune(got)) != 512 {
		t.Errorf("len = %d, want 512", len([]rune(got)))
	}
	// 600 multi-byte runes truncated to 512 must remain valid UTF-8.
	got := truncateRunes(strings.Repeat("é", 600), 512)
	if len([]rune(got)) != 512 {
		t.Errorf("rune len = %d, want 512", len([]rune(got)))
	}
	if !utf8.ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
}

// newSeriesTestEnv seeds a user + link and returns the click store, the raw
// DB (for explicit clicked_at inserts), and the link ID.
func newSeriesTestEnv(t *testing.T) (*ClickStore, *sqlx.DB, string) {
	t.Helper()
	db := testutil.NewTestDB(t)

	owns := NewOwnershipStore(db)
	tags := NewTagStore(db)
	ls := NewLinkStore(db, owns, tags)
	us := NewUserStore(db)
	cs := NewClickStore(db)

	ctx := context.Background()
	u, err := us.Upsert(ctx, "test", "series-sub", "series@example.com", "Series Tester", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	link, err := ls.Create(ctx, "series-link", "https://example.com", u.ID, "Series", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	return cs, db, link.ID
}

// seedSeriesClick inserts a click row with an explicit clicked_at
// (RecordClick stamps time.Now(), so deterministic bucketing tests need raw
// inserts).
func seedSeriesClick(t *testing.T, db *sqlx.DB, linkID string, ts time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), db.Rebind(`
		INSERT INTO link_clicks (id, link_id, user_id, ip_hash, user_agent, referrer, clicked_at)
		VALUES (?, ?, NULL, 'h', '', '', ?)
	`), uuid.New().String(), linkID, ts)
	if err != nil {
		t.Fatalf("seed click at %s: %v", ts, err)
	}
}

// Scenario: UTC day bucketing — a click recorded at 2026-07-16T23:30:00Z
// counts toward the 2026-07-16 bucket (UTC day) regardless of the viewer's
// timezone (a UTC−05:00 viewer sees 18:30 on the 16th; a UTC+01:00 viewer
// sees 00:30 on the 17th — the bucket is the UTC day either way).
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestDailyClickSeries_UTCDayBucketing(t *testing.T) {
	cs, db, linkID := newSeriesTestEnv(t)
	now := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)

	// 23:30 UTC lands on the 16th even though several viewer-local zones
	// would place it on an adjacent day — buckets are UTC days only.
	seedSeriesClick(t, db, linkID, time.Date(2026, 7, 16, 23, 30, 0, 0, time.UTC))
	// Just past UTC midnight lands on the 17th.
	seedSeriesClick(t, db, linkID, time.Date(2026, 7, 17, 0, 30, 0, 0, time.UTC))

	series, err := cs.dailyClickSeries(context.Background(), linkID, 30, 0, now)
	if err != nil {
		t.Fatalf("dailyClickSeries: %v", err)
	}
	byDate := map[string]int64{}
	for _, d := range series {
		byDate[d.Date] = d.Count
	}
	if byDate["2026-07-16"] != 1 {
		t.Errorf("2026-07-16 count = %d, want 1 (the 23:30Z click)", byDate["2026-07-16"])
	}
	if byDate["2026-07-17"] != 1 {
		t.Errorf("2026-07-17 count = %d, want 1 (the 00:30Z click)", byDate["2026-07-17"])
	}
}

// Scenario: Chart renders with gap days (store side) — a link clicked on
// only 3 of the last 30 days yields exactly 30 entries in ascending date
// order with zero-count entries present on the 27 unclicked days.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestDailyClickSeries_GapDaysZeroFilled(t *testing.T) {
	cs, db, linkID := newSeriesTestEnv(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	seedSeriesClick(t, db, linkID, time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC))
	seedSeriesClick(t, db, linkID, time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC))
	seedSeriesClick(t, db, linkID, time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC))
	seedSeriesClick(t, db, linkID, time.Date(2026, 6, 20, 23, 0, 0, 0, time.UTC))

	series, err := cs.dailyClickSeries(context.Background(), linkID, 30, 0, now)
	if err != nil {
		t.Fatalf("dailyClickSeries: %v", err)
	}
	if len(series) != 30 {
		t.Fatalf("len(series) = %d, want exactly 30", len(series))
	}
	// Ascending, consecutive UTC days from the window start to today.
	for i, d := range series {
		want := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
		if d.Date != want {
			t.Fatalf("series[%d].Date = %q, want %q (ascending gap-filled days)", i, d.Date, want)
		}
	}
	zeroDays := 0
	counts := map[string]int64{}
	for _, d := range series {
		if d.Count == 0 {
			zeroDays++
		}
		counts[d.Date] = d.Count
	}
	if zeroDays != 27 {
		t.Errorf("zero-count days = %d, want 27", zeroDays)
	}
	if counts["2026-07-17"] != 1 || counts["2026-07-10"] != 2 || counts["2026-06-20"] != 1 {
		t.Errorf("clicked-day counts wrong: %v", counts)
	}
}

// Scenario: Pruned days distinguished from zero days — retention enabled at
// 60 days with the 90-day window marks the oldest 30 day positions as
// no-data (Pruned), visually distinct from zero-count days inside the
// horizon.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", REQ "Click Retention"
func TestDailyClickSeries_PrunedDaysDistinguishedFromZeroDays(t *testing.T) {
	cs, db, linkID := newSeriesTestEnv(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// One surviving click inside the horizon.
	seedSeriesClick(t, db, linkID, time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))

	series, err := cs.dailyClickSeries(context.Background(), linkID, 90, 60, now)
	if err != nil {
		t.Fatalf("dailyClickSeries: %v", err)
	}
	if len(series) != 90 {
		t.Fatalf("len(series) = %d, want exactly 90", len(series))
	}
	pruned := 0
	for i, d := range series {
		if d.Pruned {
			pruned++
			if i >= 30 {
				t.Errorf("series[%d] (%s) marked Pruned, but only the oldest 30 positions should be", i, d.Date)
			}
		}
	}
	if pruned != 30 {
		t.Errorf("pruned days = %d, want exactly 30 (90-day window, 60-day horizon)", pruned)
	}
	// A zero-count day inside the horizon is a zero day, not a pruned day.
	for _, d := range series[30:] {
		if d.Pruned {
			t.Errorf("day %s inside the horizon marked Pruned", d.Date)
		}
	}
	// Retention off (0) never marks anything pruned.
	series, err = cs.dailyClickSeries(context.Background(), linkID, 90, 0, now)
	if err != nil {
		t.Fatalf("dailyClickSeries retention-off: %v", err)
	}
	for _, d := range series {
		if d.Pruned {
			t.Errorf("retention disabled but day %s marked Pruned", d.Date)
		}
	}
}

// The SQL range predicate is aligned to the UTC midnight opening the oldest
// bucket — never a rolling now−30d bound: a click at that exact midnight is
// included; a click one second earlier is excluded even though it is within
// rolling now−30d.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestDailyClickSeries_WindowAlignedToUTCMidnight(t *testing.T) {
	cs, db, linkID := newSeriesTestEnv(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	windowStart := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)

	seedSeriesClick(t, db, linkID, windowStart)                   // exact opening midnight: in
	seedSeriesClick(t, db, linkID, windowStart.Add(-time.Second)) // 2026-06-17T23:59:59Z: out
	seedSeriesClick(t, db, linkID, windowStart.Add(6*time.Hour))  // same oldest day: in

	series, err := cs.dailyClickSeries(context.Background(), linkID, 30, 0, now)
	if err != nil {
		t.Fatalf("dailyClickSeries: %v", err)
	}
	if series[0].Date != "2026-06-18" {
		t.Fatalf("series[0].Date = %q, want 2026-06-18", series[0].Date)
	}
	if series[0].Count != 2 {
		t.Errorf("oldest bucket count = %d, want 2 (midnight click included, 23:59:59 excluded)", series[0].Count)
	}
	var total int64
	for _, d := range series {
		total += d.Count
	}
	if total != 2 {
		t.Errorf("total clicks in window = %d, want 2", total)
	}
}

// Windows other than 30 and 90 are rejected at the store layer too (the API
// maps this policy to 400, the web UI falls back to 30 before calling in).
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestDailyClickSeries_InvalidWindowRejected(t *testing.T) {
	cs, _, linkID := newSeriesTestEnv(t)
	for _, days := range []int{0, 7, 29, 31, 365, -30} {
		if _, err := cs.dailyClickSeries(context.Background(), linkID, days, 0, time.Now().UTC()); err == nil {
			t.Errorf("days=%d accepted, want error", days)
		}
	}
}

// GetDailyClickSeries (real clock) ends on today's UTC day and returns
// exactly the requested number of buckets.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
func TestGetDailyClickSeries_EndsOnTodayUTC(t *testing.T) {
	cs, _, linkID := newSeriesTestEnv(t)

	before := time.Now().UTC().Format("2006-01-02")
	series, err := cs.GetDailyClickSeries(context.Background(), linkID, 30, 0)
	if err != nil {
		t.Fatalf("GetDailyClickSeries: %v", err)
	}
	after := time.Now().UTC().Format("2006-01-02")

	if len(series) != 30 {
		t.Fatalf("len(series) = %d, want 30", len(series))
	}
	last := series[len(series)-1].Date
	if last != before && last != after {
		t.Errorf("newest bucket = %q, want today's UTC day (%q or %q)", last, before, after)
	}
}
