// Governing: SPEC-0004 REQ "Tag Browser", "New Link Form", ADR-0007
package handler

import (
	"html"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// TagsHandler serves tag browsing views.
type TagsHandler struct {
	tags     *store.TagStore
	links    *store.LinkStore
	keywords *store.KeywordStore
}

// NewTagsHandler creates a new TagsHandler.
func NewTagsHandler(ts *store.TagStore, ls *store.LinkStore, ks *store.KeywordStore) *TagsHandler {
	return &TagsHandler{tags: ts, links: ls, keywords: ks}
}

// TagIndexPage is the template data for the tag browser.
type TagIndexPage struct {
	BasePage
	Tags []*store.TagWithCount
}

// TagDetailPage is the template data for the tag detail view.
type TagDetailPage struct {
	BasePage
	Tag            *store.Tag
	Links          []*store.Link
	Query          string // unused; present for shared link_list partial compatibility
	ShowVisibility bool
	ShowActions    bool
	ShowTitle      bool
	ShowOwner      bool
	ShowTags       bool
}

// visibleUserID returns the user's ID, or "" for anonymous viewers so
// visibility-filtered store queries match public links only.
func visibleUserID(user *store.User) string {
	if user == nil {
		return ""
	}
	return user.ID
}

// Index renders all tags with ≥1 link and their counts.
// Governing: SPEC-0004 REQ "Tag Browser"
func (h *TagsHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering", REQ "Admin Visibility Override"
	var tags []*store.TagWithCount
	if user != nil && user.IsAdmin() {
		tags, _ = h.tags.ListWithCounts(r.Context())
	} else {
		tags, _ = h.tags.ListWithCountsVisible(r.Context(), visibleUserID(user))
	}
	data := TagIndexPage{
		BasePage: newBasePage(r, user),
		Tags:     tags,
	}
	if isHTMX(r) {
		renderPageFragment(w, "tags/index.html", "content", data)
		return
	}
	render(w, "tags/index.html", data)
}

// Detail renders links for a specific tag.
// Governing: SPEC-0004 REQ "Tag Browser"
func (h *TagsHandler) Detail(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	slug := chi.URLParam(r, "slug")
	tag, err := h.tags.GetBySlug(r.Context(), slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering", REQ "Admin Visibility Override"
	var links []*store.Link
	if user != nil && user.IsAdmin() {
		links, _ = h.links.ListByTag(r.Context(), slug)
	} else {
		links, _ = h.links.ListVisibleByTag(r.Context(), slug, visibleUserID(user))
		// A tag whose links are all invisible to the viewer must be
		// indistinguishable from a tag that does not exist: rendering the
		// page would confirm the tag's existence and canonical display name
		// (a tag-existence oracle for slug probing). The visibility-filtered
		// Index omits such tags entirely; Detail must match.
		// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
		if len(links) == 0 {
			http.NotFound(w, r)
			return
		}
	}

	data := TagDetailPage{
		BasePage:       newBasePage(r, user),
		Tag:            tag,
		Links:          links,
		ShowVisibility: true,
		ShowActions:    true,
		ShowTitle:      true,
		ShowOwner:      false,
		ShowTags:       false,
	}
	if isHTMX(r) {
		renderPageFragment(w, "tags/detail.html", "content", data)
		return
	}
	render(w, "tags/detail.html", data)
}

// Suggest returns tag autocomplete results as HTML options.
// Governing: SPEC-0004 REQ "New Link Form" — tag autocomplete via HTMX
func (h *TagsHandler) Suggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	w.Header().Set("Content-Type", "text/html")
	if q == "" {
		_, _ = w.Write([]byte(""))
		return
	}
	tags, err := h.tags.SearchByPrefix(r.Context(), q)
	if err != nil || len(tags) == 0 {
		_, _ = w.Write([]byte(""))
		return
	}
	var buf []byte
	for _, t := range tags {
		buf = append(buf, []byte(`<li><button type="button" class="btn btn-ghost btn-sm justify-start" onclick="addTag('`+html.EscapeString(t.Name)+`')">`+html.EscapeString(t.Name)+`</button></li>`)...)
	}
	_, _ = w.Write(buf)
}
