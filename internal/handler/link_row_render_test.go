package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/store"
)

// Governing: SPEC-0014 REQ "Abstract Link Widget" — verify the shared link_row
// partial and admin/profile renderers execute against their real data shapes.
func sampleAdminLink() *store.AdminLink {
	al := &store.AdminLink{
		Owners:    "Alice",
		Tags:      "go,docs",
		OwnerSlug: "alice",
		IsOwner:   true,
	}
	al.ID = "00000000-0000-0000-0000-000000000001"
	al.Slug = "example"
	al.URL = "https://example.com/$page"
	al.Title = "Example"
	al.Description = "An example link"
	al.Visibility = "public"
	al.CreatedAt = time.Now()
	return al
}

func TestLinkRowRenders_AdminAndDashboard(t *testing.T) {
	link := sampleAdminLink()

	cases := []struct {
		name      string
		path      string
		wantHrefs []string
	}{
		{"admin", "/admin/links/1/row", []string{"/admin/links/", "/admin/links/" + link.ID + "/confirm-delete"}},
		{"dashboard", "/dashboard", []string{"/dashboard/links/" + link.ID, "/dashboard/links/" + link.ID + "/stats", "/dashboard/links/" + link.ID + "/confirm-delete"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.path, nil)
			// Governing: SPEC-0010 — non-admin rows read per-row capabilities
			// from RowCaps, so the dashboard case uses DashboardPage.
			var ctx any = AdminLinksPage{
				BasePage:       newBasePage(r, nil),
				ShowTitle:      true,
				ShowOwner:      true,
				ShowTags:       true,
				ShowVisibility: true,
				ShowActions:    true,
			}
			if tc.name == "dashboard" {
				ctx = DashboardPage{
					BasePage:       newBasePage(r, nil),
					ShowTitle:      true,
					ShowOwner:      true,
					ShowTags:       true,
					ShowVisibility: true,
					ShowActions:    true,
					RowCaps: map[string]store.LinkCaps{
						link.ID: store.NewLinkCaps(true, false, false),
					},
				}
			}
			rr := httptest.NewRecorder()
			renderFragment(rr, "link_row", map[string]any{"Link": link, "Ctx": ctx})
			body := rr.Body.String()
			if !strings.Contains(body, `id="link-`+link.ID+`"`) {
				t.Fatalf("missing row id; body=%s", body)
			}
			// shared copy button + keyword prefix present
			if !strings.Contains(body, "Copy link") {
				t.Errorf("missing copy button")
			}
			for _, h := range tc.wantHrefs {
				if !strings.Contains(body, h) {
					t.Errorf("expected %q in body; body=%s", h, body)
				}
			}
		})
	}
}

func TestAdminEditRowRenders(t *testing.T) {
	link := sampleAdminLink()
	r := httptest.NewRequest("GET", "/admin/links/1/edit", nil)
	// Built inline: adminLinkRowData is now an AdminHandler method (it also
	// derives the row's health state), and this is a pure render test.
	data := AdminLinkRowData{
		Link: link,
		Ctx: AdminLinksPage{
			BasePage:       newBasePage(r, nil),
			ShowTitle:      true,
			ShowOwner:      true,
			ShowTags:       true,
			ShowVisibility: true,
			ShowActions:    true,
			ShowLifecycle:  true,
		},
	}
	rr := httptest.NewRecorder()
	renderPageFragment(rr, "admin/links.html", "admin_link_edit_row", data)
	body := rr.Body.String()
	if !strings.Contains(body, `id="link-`+link.ID+`"`) {
		t.Fatalf("edit row id mismatch; body=%s", body)
	}
	if !strings.Contains(body, `hx-target="#link-`+link.ID+`"`) {
		t.Errorf("edit row save/cancel should target #link-<id>; body=%s", body)
	}
}

func TestProfilePageRenders(t *testing.T) {
	link := sampleAdminLink()
	r := httptest.NewRequest("GET", "/u/alice", nil)
	data := ProfilePage{
		BasePage:       newBasePage(r, nil),
		ProfileUser:    &store.User{DisplayName: "Alice", DisplayNameSlug: "alice"},
		Links:          []*store.AdminLink{link},
		Page:           1,
		TotalPages:     1,
		TotalLinks:     1,
		ShowOwner:      true,
		ShowTags:       true,
		ShowVisibility: true,
	}
	rr := httptest.NewRecorder()
	render(rr, "profile.html", data)
	body := rr.Body.String()
	if !strings.Contains(body, "table") || !strings.Contains(body, link.Slug) {
		t.Fatalf("profile page should render link_list table with the link; body=%s", body)
	}
}

// Governing: SPEC-0014 REQ "Abstract Link Widget" — every page struct that feeds
// the shared link_list partial must render its empty-state branch, which
// evaluates $.Query and $.Tag. A struct missing either field crashes mid-render
// (issue #194: TagDetailPage lacked Query, so tags with zero links 500'd).
func TestLinkListEmptyState_AllPageStructs(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		data     func(r *http.Request) any
		want     string
		dontWant string
	}{
		{
			name: "tag detail with zero links",
			path: "/tags/dead-tag",
			data: func(r *http.Request) any {
				return TagDetailPage{
					BasePage: newBasePage(r, nil),
					Tag:      &store.Tag{ID: "t1", Name: "dead-tag", Slug: "dead-tag"},
				}
			},
			want: "No matching links",
		},
		{
			name: "dashboard with no links",
			path: "/dashboard",
			data: func(r *http.Request) any {
				return DashboardPage{BasePage: newBasePage(r, nil)}
			},
			want: "No links yet",
		},
		{
			name: "dashboard with query and no results",
			path: "/dashboard?q=nope",
			data: func(r *http.Request) any {
				return DashboardPage{BasePage: newBasePage(r, nil), Query: "nope"}
			},
			want: "No matching links",
		},
		{
			name: "admin with no links",
			path: "/admin/links",
			data: func(r *http.Request) any {
				return AdminLinksPage{BasePage: newBasePage(r, nil)}
			},
			want:     "No links have been created yet",
			dontWant: "Create a link",
		},
		{
			name: "profile with no links",
			path: "/u/alice",
			data: func(r *http.Request) any {
				return ProfilePage{BasePage: newBasePage(r, nil)}
			},
			want: "No links yet",
		},
		{
			name: "public browser with no results",
			path: "/links?q=nope",
			data: func(r *http.Request) any {
				return PublicLinksPage{BasePage: newBasePage(r, nil), Query: "nope"}
			},
			want: "No matching links",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.path, nil)
			rr := httptest.NewRecorder()
			renderFragment(rr, "link_list", tc.data(r))
			body := rr.Body.String()
			if strings.Contains(body, "template error") {
				t.Fatalf("link_list crashed on empty %s: %s", tc.name, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("expected empty-state copy %q; body=%s", tc.want, body)
			}
			if tc.dontWant != "" && strings.Contains(body, tc.dontWant) {
				t.Errorf("unexpected copy %q; body=%s", tc.dontWant, body)
			}
		})
	}
}

// The lifecycle badge is gated on Ctx.ShowLifecycle: capability surfaces
// (dashboard, admin, tag detail) render it; the anonymous public browser and
// profile pages must not — SPEC-0020 excludes expired/archived links from
// those surfaces entirely (the exclusion itself is #274), so leaking the badge
// there would disclose exactly what the archived 404 path hides.
// Governing: SPEC-0020 REQ "Health Badges and Admin Report", Security "Resolution Ordering and Oracle Resistance"
func TestLifecycleBadge_GatedToCapabilitySurfaces(t *testing.T) {
	archived := sampleAdminLink()
	past := time.Now().UTC().Add(-time.Hour)
	archived.ArchivedAt = &past

	cases := []struct {
		name      string
		ctx       any
		wantBadge bool
	}{
		{"dashboard shows badge", DashboardPage{ShowVisibility: true, ShowLifecycle: true}, true},
		{"admin shows badge", AdminLinksPage{ShowVisibility: true, ShowLifecycle: true}, true},
		{"tag detail shows badge", TagDetailPage{ShowVisibility: true, ShowLifecycle: true}, true},
		{"public browser hides badge", PublicLinksPage{ShowVisibility: true}, false},
		{"profile hides badge", ProfilePage{ShowVisibility: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			renderFragment(rr, "link_row", map[string]any{"Link": archived, "Ctx": tc.ctx})
			body := rr.Body.String()
			if strings.Contains(body, "template error") {
				t.Fatalf("link_row crashed: %s", body)
			}
			got := strings.Contains(body, ">archived</span>")
			if got != tc.wantBadge {
				t.Errorf("archived badge rendered = %v, want %v; body=%s", got, tc.wantBadge, body)
			}
		})
	}
}

// Governing: SPEC-0014 — the dashboard renders link_list with []*store.Link (no
// AdminLink owner/tag fields). Guards must keep those fields un-evaluated so the
// dashboard never 500s on the shared partial.
func TestDashboardLinkListRenders_StoreLink(t *testing.T) {
	r := httptest.NewRequest("GET", "/dashboard", nil)
	l := &store.Link{
		ID:          "00000000-0000-0000-0000-0000000000aa",
		Slug:        "dash-link",
		URL:         "https://example.com",
		Title:       "Dash",
		Description: "d",
		Visibility:  "public",
		CreatedAt:   time.Now(),
	}
	data := DashboardPage{
		BasePage: newBasePage(r, nil),
		Links:    []*store.Link{l},
		// matches DashboardHandler.Show; all other Show* false
		ShowVisibility: true,
		ShowActions:    true,
		// Governing: SPEC-0010 — owner rows get full per-row capabilities
		RowCaps: map[string]store.LinkCaps{l.ID: store.NewLinkCaps(true, false, false)},
	}
	rr := httptest.NewRecorder()
	renderFragment(rr, "link_list", data)
	body := rr.Body.String()
	if !strings.Contains(body, `id="link-`+l.ID+`"`) {
		t.Fatalf("dashboard link_list missing row; body=%s", body)
	}
	if !strings.Contains(body, "/dashboard/links/"+l.ID+"/stats") {
		t.Errorf("dashboard row should show stats action; body=%s", body)
	}
	// Governing: SPEC-0010 — the owner's own dashboard badges visibility (#206)
	if !strings.Contains(body, "<th>Visibility</th>") || !strings.Contains(body, "badge-ghost") {
		t.Errorf("dashboard link_list should render the Visibility column with a badge; body=%s", body)
	}
}
