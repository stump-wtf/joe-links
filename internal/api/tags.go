// Governing: SPEC-0005 REQ "Tags", ADR-0008
package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// tagsAPIHandler provides REST handlers for tag endpoints.
// Governing: SPEC-0005 REQ "Tags"
type tagsAPIHandler struct {
	tags      *store.TagStore
	links     *store.LinkStore
	ownership *store.OwnershipStore
}

// registerTagRoutes registers tag routes on r.
// Governing: SPEC-0005 REQ "Tags"
func registerTagRoutes(r chi.Router, tags *store.TagStore, links *store.LinkStore, ownership *store.OwnershipStore) {
	h := &tagsAPIHandler{tags: tags, links: links, ownership: ownership}
	r.Get("/tags", h.List)
	r.Get("/tags/{slug}/links", h.ListLinks)
}

// List returns all tags with link_count >= 1.
// GET /api/v1/tags
// Governing: SPEC-0005 REQ "Tags" — tags with link_count = 0 MUST NOT appear.
//
// @Summary      List tags
// @Description  Returns tags that have at least one associated link visible to the caller (public, owned, co-owned, or shared). Link counts include only visible links. Admins see all tags with full counts.
// @Tags         Tags
// @Accept       json
// @Produce      json
// @Param        limit   query     int     false  "Max items to return (default 50, max 200)"
// @Param        cursor  query     string  false  "Opaque pagination cursor from a prior next_cursor"
// @Success      200  {object}  TagListResponse
// @Failure      401  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /tags [get]
func (h *tagsAPIHandler) List(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	// Governing: SPEC-0005 REQ "Pagination" — ?limit (default 50, max 200) + opaque ?cursor
	limit := parseLimit(r)
	cursorName, cursorID := parseCursor(r)

	// Fetch limit+1 to detect whether another page exists.
	// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering", REQ "Admin Visibility Override"
	// — non-admins must not enumerate tag names or counts derived from links
	// invisible to them (issue #244).
	var tagsWithCounts []*store.TagWithCount
	var err error
	if user.IsAdmin() {
		tagsWithCounts, err = h.tags.ListWithCountsPaginated(r.Context(), limit+1, cursorName, cursorID)
	} else {
		tagsWithCounts, err = h.tags.ListWithCountsVisiblePaginated(r.Context(), user.ID, limit+1, cursorName, cursorID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	var nextCursor *string
	if len(tagsWithCounts) > limit {
		last := tagsWithCounts[limit-1]
		c := encodeCursor(last.Name, last.ID)
		nextCursor = &c
		tagsWithCounts = tagsWithCounts[:limit]
	}

	resp := &TagListResponse{Tags: make([]*TagResponse, 0, len(tagsWithCounts)), NextCursor: nextCursor}
	for _, t := range tagsWithCounts {
		resp.Tags = append(resp.Tags, &TagResponse{
			Slug:      t.Slug,
			Name:      t.Name,
			LinkCount: t.Count,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// ListLinks returns links tagged with the given slug.
// GET /api/v1/tags/{slug}/links
// Governing: SPEC-0005 REQ "Tags" — admin sees all links; non-admin sees only visible links.
//
// @Summary      List links by tag
// @Description  Returns links with the given tag. Admins see all; non-admins see only links visible to them (public, owned, co-owned, or shared). Returns 404 when the tag does not exist — or, for non-admins, when it has no visible links.
// @Tags         Tags
// @Accept       json
// @Produce      json
// @Param        slug  path      string  true  "Tag slug"
// @Success      200   {object}  LinkListResponse
// @Failure      401   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     BearerToken
// @Router       /tags/{slug}/links [get]
func (h *tagsAPIHandler) ListLinks(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	tagSlug := chi.URLParam(r, "slug")

	var links []*store.Link
	var err error
	if user.IsAdmin() {
		// Governing: SPEC-0010 REQ "Admin Visibility Override"
		// Verify the tag exists.
		_, err = h.tags.GetBySlug(r.Context(), tagSlug)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "tag not found", CodeNotFound)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
			return
		}
		links, err = h.links.ListByTag(r.Context(), tagSlug)
	} else {
		// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering" — non-admins
		// see public, owned/co-owned, and shared links only (issue #244).
		links, err = h.links.ListVisibleByTag(r.Context(), tagSlug, user.ID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}
	// A tag whose links are all invisible to the caller must be
	// indistinguishable from a tag that does not exist: returning 200 with an
	// empty list would confirm the tag's existence by slug probing (a
	// tag-existence oracle). Mirrors the dashboard Detail behavior from PR #241.
	// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
	if !user.IsAdmin() && len(links) == 0 {
		writeError(w, http.StatusNotFound, "tag not found", CodeNotFound)
		return
	}

	// Non-admin tag lists include public links the caller holds no
	// capabilities on, so the health object is capability-gated per row.
	// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP" scenario
	// "Non-Capable Caller Gets No Health Data"
	ids := make([]string, len(links))
	for i, l := range links {
		ids[i] = l.ID
	}
	rowCaps, err := store.LinkCapsForAll(r.Context(), h.ownership, h.links, ids, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	// Governing: SPEC-0005 REQ "API Response Structures" — consistent link shape (owners, tags, visibility)
	resp := &LinkListResponse{Links: make([]*LinkResponse, 0, len(links))}
	for _, l := range links {
		lr, err := buildLinkResponse(r.Context(), h.links, h.ownership, l, rowCaps[l.ID].CanView)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
			return
		}
		resp.Links = append(resp.Links, lr)
	}

	writeJSON(w, http.StatusOK, resp)
}
