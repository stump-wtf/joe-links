package handler

// UI polish batch regression tests (issue #206): visibility badge on the link
// detail page, footer build tooltip de-duplication, and UTC-labeled timestamps.

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/build"
	"github.com/joestump/joe-links/internal/store"
)

// TestLinkDetailRenders_VisibilityBadgeAndUTC verifies the detail page badges
// the link's visibility and labels its timestamps as UTC (#206).
// Governing: SPEC-0004 REQ "Link Detail View"
func TestLinkDetailRenders_VisibilityBadgeAndUTC(t *testing.T) {
	cases := []struct {
		visibility string
		wantBadge  string
	}{
		{"public", "badge-ghost"},
		{"private", "badge-warning"},
		{"secure", "badge-error"},
	}
	for _, tc := range cases {
		t.Run(tc.visibility, func(t *testing.T) {
			l := &store.Link{
				ID:         "00000000-0000-0000-0000-0000000000bb",
				Slug:       "detail-link",
				URL:        "https://example.com",
				Visibility: tc.visibility,
				// Non-UTC zone: the template must convert before labeling UTC.
				CreatedAt: time.Date(2026, 7, 12, 2, 40, 0, 0, time.FixedZone("PDT", -8*3600)),
				UpdatedAt: time.Date(2026, 7, 12, 2, 40, 0, 0, time.FixedZone("PDT", -8*3600)),
			}
			r := httptest.NewRequest("GET", "/dashboard/links/"+l.ID, nil)
			data := LinkDetailPage{
				BasePage: newBasePage(r, nil),
				Link:     l,
			}
			rr := httptest.NewRecorder()
			render(rr, "links/detail.html", data)
			body := rr.Body.String()
			if strings.Contains(body, "template error") {
				t.Fatalf("detail page crashed: %s", body)
			}
			if !strings.Contains(body, tc.wantBadge) || !strings.Contains(body, ">"+tc.visibility+"<") {
				t.Errorf("detail page should badge %s visibility with %s; body=%s", tc.visibility, tc.wantBadge, body)
			}
			// 02:40 -08:00 == 10:40 UTC; the label must follow the conversion.
			if !strings.Contains(body, "Created Jul 12, 2026 10:40 AM UTC") {
				t.Errorf("detail page should render UTC-converted, UTC-labeled Created timestamp; body=%s", body)
			}
		})
	}
}

// TestStatsPageRenders_UTCTimestamps verifies recent-click timestamps are
// converted to UTC before being labeled UTC, with an RFC 3339 title (#206).
// Governing: SPEC-0016 REQ "Link Stats Dashboard Page"
func TestStatsPageRenders_UTCTimestamps(t *testing.T) {
	l := &store.Link{
		ID:         "00000000-0000-0000-0000-0000000000cc",
		Slug:       "stats-link",
		URL:        "https://example.com",
		Visibility: "public",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	r := httptest.NewRequest("GET", "/dashboard/links/"+l.ID+"/stats", nil)
	data := StatsPage{
		BasePage: newBasePage(r, nil),
		Link:     l,
		RecentClicks: []store.RecentClick{{
			ID: "c1",
			// 02:40 -08:00 == 10:40 UTC
			ClickedAt: time.Date(2026, 7, 12, 2, 40, 0, 0, time.FixedZone("PDT", -8*3600)),
			Referrer:  "https://ref.example.com",
		}},
	}
	rr := httptest.NewRecorder()
	render(rr, "links/stats.html", data)
	body := rr.Body.String()
	if strings.Contains(body, "template error") {
		t.Fatalf("stats page crashed: %s", body)
	}
	if !strings.Contains(body, "Jul 12, 2026 10:40 AM UTC") {
		t.Errorf("stats page should render the UTC-converted, UTC-labeled click time; body=%s", body)
	}
	if !strings.Contains(body, `title="2026-07-12T10:40:00Z"`) {
		t.Errorf("stats click time should carry an RFC 3339 title attribute; body=%s", body)
	}
}

// TestNewBasePage_DropsBranchWhenEqualToVersion verifies the footer tooltip
// fix: CI stamps BRANCH with the tag name on tag builds, so the redundant
// segment is dropped rather than shown twice (#206).
func TestNewBasePage_DropsBranchWhenEqualToVersion(t *testing.T) {
	origVersion, origBranch := build.Version, build.Branch
	t.Cleanup(func() { build.Version, build.Branch = origVersion, origBranch })

	r := httptest.NewRequest("GET", "/dashboard", nil)

	// Tag build: branch duplicates version → dropped.
	build.Version, build.Branch = "v0.5.3", "v0.5.3"
	if got := newBasePage(r, nil); got.BuildBranch != "" {
		t.Errorf("BuildBranch should be empty when it equals the version; got %q", got.BuildBranch)
	}

	// Branch build: real branch name is kept.
	build.Version, build.Branch = "dev", "main"
	if got := newBasePage(r, nil); got.BuildBranch != "main" {
		t.Errorf("BuildBranch should be preserved for branch builds; got %q", got.BuildBranch)
	}
}
