// Governing: SPEC-0001 REQ "Short Link Management", REQ "HTMX Hypermedia Interactions", ADR-0001
// Governing: SPEC-0003 REQ "Theme Persistence via Cookie", ADR-0006
// Governing: SPEC-0004 REQ "User Dashboard"
// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// DashboardPage is the template data for the dashboard view.
// Governing: SPEC-0014 REQ "Abstract Link Widget"
// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
type DashboardPage struct {
	BasePage
	User           *store.User
	Links          []*store.Link
	Tags           []*store.Tag
	Query          string // current search query
	Tag            string // current tag filter slug
	Filter         string // "shared" for shared-with-me view
	Flash          *Flash
	ShowTitle      bool // show Title column
	ShowOwner      bool // show Owner(s) column
	ShowTags       bool // show Tags column
	ShowVisibility bool // show Visibility column
	ShowActions    bool // show the actions column at all
	// ShowLifecycle gates the expired/archived badge to capability surfaces —
	// public browser and profile pages must not disclose lifecycle state.
	// Governing: SPEC-0020 REQ "Expired Link Resolution", REQ "Health Badges and Admin Report"
	ShowLifecycle bool
	// HealthStates maps link ID → derived destination-health state for rows
	// the viewer holds capabilities on; nil on public surfaces, which must
	// never display health information.
	// Governing: SPEC-0020 REQ "Health Badges and Admin Report"
	HealthStates map[string]string
	// RowCaps maps link ID → the viewer's capabilities for that row, so lists
	// mixing rows with differing rights (e.g. ?filter=shared) never render
	// dead action buttons.
	// Governing: SPEC-0010 REQ "Link Shares Table" — recipients are read-only
	RowCaps map[string]store.LinkCaps
}

// DashboardHandler serves the authenticated link management dashboard.
type DashboardHandler struct {
	links    *store.LinkStore
	owns     *store.OwnershipStore
	tags     *store.TagStore
	keywords *store.KeywordStore
}

// NewDashboardHandler creates a new DashboardHandler.
// Governing: SPEC-0004 REQ "User Dashboard"
func NewDashboardHandler(ls *store.LinkStore, os *store.OwnershipStore, ts *store.TagStore, ks *store.KeywordStore) *DashboardHandler {
	return &DashboardHandler{links: ls, owns: os, tags: ts, keywords: ks}
}

// buildRowCaps resolves per-row link capabilities for the viewer.
// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
// Governing: SPEC-0010 REQ "Link Shares Table"
func buildRowCaps(ctx context.Context, owns *store.OwnershipStore, ls *store.LinkStore, user *store.User, links []*store.Link) (map[string]store.LinkCaps, error) {
	ids := make([]string, len(links))
	for i, l := range links {
		ids[i] = l.ID
	}
	return store.LinkCapsForAll(ctx, owns, ls, ids, user)
}

// Show renders the dashboard with the user's links (or all links for admins).
// Supports ?q= for search and ?tag= for tag filtering via HTMX.
// Governing: SPEC-0004 REQ "User Dashboard"
// Governing: SPEC-0001 REQ "HTMX Hypermedia Interactions"
func (h *DashboardHandler) Show(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	query := r.URL.Query().Get("q")
	tagSlug := r.URL.Query().Get("tag")
	filter := r.URL.Query().Get("filter")

	var links []*store.Link
	var err error

	switch {
	// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — "Shared with me" filter
	case filter == "shared":
		links, err = h.links.ListSharedWithUser(r.Context(), user.ID)
	// Staleness views, computed from link_clicks at query time in the store
	// layer over the viewer's own dashboard scope (all links for admins,
	// owned links otherwise). Archived links are out of scope.
	// Governing: SPEC-0020 REQ "Staleness Views" scenarios "Stale Filter",
	// "Never-Clicked Filter", "Staleness Respects Visibility Scope"
	case filter == "stale":
		if user.IsAdmin() {
			links, err = h.links.ListStaleAll(r.Context(), time.Now().UTC())
		} else {
			links, err = h.links.ListStaleByOwner(r.Context(), user.ID, time.Now().UTC())
		}
	case filter == "never-clicked":
		if user.IsAdmin() {
			links, err = h.links.ListNeverClickedAll(r.Context(), time.Now().UTC())
		} else {
			links, err = h.links.ListNeverClickedByOwner(r.Context(), user.ID, time.Now().UTC())
		}
	case tagSlug != "":
		// Tag filter takes precedence
		if user.IsAdmin() {
			links, err = h.links.ListByTag(r.Context(), tagSlug)
		} else {
			links, err = h.links.ListByOwnerAndTag(r.Context(), user.ID, tagSlug)
		}
	case query != "":
		// Search filter
		if user.IsAdmin() {
			links, err = h.links.SearchAll(r.Context(), query)
		} else {
			links, err = h.links.SearchByOwner(r.Context(), user.ID, query)
		}
	default:
		// No filters
		if user.IsAdmin() {
			links, err = h.links.ListAll(r.Context())
		} else {
			links, err = h.links.ListByOwner(r.Context(), user.ID)
		}
	}
	if err != nil {
		http.Error(w, "could not load links", http.StatusInternalServerError)
		return
	}

	// Load all tags for the tag filter chips
	allTags, _ := h.tags.ListAll(r.Context())

	// Per-row capabilities: the "Shared with me" filter (and admin's all-links
	// view) mixes rows the viewer can and cannot act on, so each row renders
	// only the actions the viewer may actually perform.
	// Governing: SPEC-0010 REQ "Link Shares Table" — recipients are read-only
	rowCaps, err := buildRowCaps(r.Context(), h.owns, h.links, user, links)
	if err != nil {
		http.Error(w, "could not load links", http.StatusInternalServerError)
		return
	}

	// Per-row derived health for the "broken" badge. Every dashboard row is
	// one the viewer holds capabilities on (owned, shared, or admin), and the
	// capability gate is enforced per row anyway.
	// Governing: SPEC-0020 REQ "Health Badges and Admin Report" scenario
	// "Broken Badge on Owner Dashboard"
	healthStates, err := buildHealthStates(r.Context(), h.links, links, rowCaps, time.Now().UTC())
	if err != nil {
		http.Error(w, "could not load links", http.StatusInternalServerError)
		return
	}

	data := DashboardPage{
		BasePage: newBasePage(r, user),
		User:     user,
		Links:    links,
		Tags:     allTags,
		Query:    query,
		Tag:      tagSlug,
		Filter:   filter,
		// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — badge each
		// row so owners can tell their secure/private links from public ones at
		// a glance (issue #206).
		ShowVisibility: true,
		ShowActions:    true,
		// Governing: SPEC-0020 REQ "Expired Link Resolution" scenario "Owner
		// Sees Expired Badge on Dashboard" — the dashboard is a capability
		// surface; lifecycle badges render here.
		ShowLifecycle: true,
		HealthStates:  healthStates,
		RowCaps:       rowCaps,
	}

	if isHTMX(r) {
		renderFragment(w, "link_list", data)
		return
	}
	render(w, "dashboard.html", data)
}
