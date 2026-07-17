// Governing: SPEC-0005 REQ "Links Collection", REQ "Link Resource", REQ "Co-Owner Management", ADR-0008
// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
// Governing: SPEC-0010 REQ "REST API Visibility Field"
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// linksAPIHandler provides REST handlers for link management.
// Governing: SPEC-0005 REQ "Links Collection", REQ "Link Resource"
type linksAPIHandler struct {
	links     *store.LinkStore
	ownership *store.OwnershipStore
	users     *store.UserStore
}

// registerLinkRoutes registers link and co-owner routes on r.
// Governing: SPEC-0005 REQ "Links Collection", REQ "Link Resource", REQ "Co-Owner Management"
func registerLinkRoutes(r chi.Router, links *store.LinkStore, ownership *store.OwnershipStore, users *store.UserStore) {
	h := &linksAPIHandler{links: links, ownership: ownership, users: users}
	r.Get("/links", h.List)
	r.Post("/links", h.Create)
	r.Get("/links/{id}", h.Get)
	r.Put("/links/{id}", h.Update)
	r.Delete("/links/{id}", h.Delete)
	r.Get("/links/{id}/owners", h.ListOwners)
	r.Post("/links/{id}/owners", h.AddOwner)
	r.Delete("/links/{id}/owners/{uid}", h.RemoveOwner)
}

// requireOwnerOrAdmin writes a 403 (or 500) and returns false unless the caller
// may mutate the link: owner, co-owner, or admin. Share recipients are
// read-only and never pass this gate.
// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
func requireOwnerOrAdmin(w http.ResponseWriter, ownership *store.OwnershipStore, user *store.User, linkID string) bool {
	allowed, err := store.IsOwnerOrAdmin(ownership, linkID, user.ID, user.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "forbidden", CodeForbidden)
		return false
	}
	return true
}

// List returns owned links for regular users, or all links for admins.
// GET /api/v1/links
// Governing: SPEC-0005 REQ "Links Collection"
//
// @Summary      List links
// @Description  Returns links owned by the caller. Admins see all links.
// @Tags         Links
// @Accept       json
// @Produce      json
// @Param        limit   query     int     false  "Max items to return (default 50, max 200)"
// @Param        cursor  query     string  false  "Opaque pagination cursor from a prior next_cursor"
// @Success      200  {object}  LinkListResponse
// @Failure      401  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links [get]
func (h *linksAPIHandler) List(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	// Governing: SPEC-0005 REQ "Pagination" — ?limit (default 50, max 200) + opaque ?cursor
	limit := parseLimit(r)
	cursorSlug, cursorID := parseCursor(r)

	// The ?url= filter is an exact-match lookup that returns a bounded set; it is
	// served unpaginated (next_cursor stays null).
	// Governing: SPEC-0010 REQ "REST API Visibility Field" — non-admin sees owned + shared
	if urlFilter := r.URL.Query().Get("url"); urlFilter != "" {
		links, err := h.links.ListByURL(r.Context(), urlFilter, user.ID, user.Role == "admin")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
			return
		}
		resp := &LinkListResponse{Links: make([]*LinkResponse, 0, len(links))}
		for _, l := range links {
			lr, err := h.toLinkResponse(r.Context(), l)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
				return
			}
			resp.Links = append(resp.Links, lr)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Fetch limit+1 to detect whether another page exists.
	var links []*store.Link
	var err error
	if user.Role == "admin" {
		links, err = h.links.ListAllPaginated(r.Context(), limit+1, cursorSlug, cursorID)
	} else {
		links, err = h.links.ListByOwnerOrSharedPaginated(r.Context(), user.ID, limit+1, cursorSlug, cursorID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	var nextCursor *string
	if len(links) > limit {
		last := links[limit-1]
		c := encodeCursor(last.Slug, last.ID)
		nextCursor = &c
		links = links[:limit]
	}

	resp := &LinkListResponse{Links: make([]*LinkResponse, 0, len(links)), NextCursor: nextCursor}
	for _, l := range links {
		lr, err := h.toLinkResponse(r.Context(), l)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
			return
		}
		resp.Links = append(resp.Links, lr)
	}

	writeJSON(w, http.StatusOK, resp)
}

// Create creates a new link with the authenticated user as primary owner.
// POST /api/v1/links
// Governing: SPEC-0005 REQ "Links Collection"
//
// @Summary      Create a link
// @Description  Creates a new short link. The caller becomes the primary owner.
// @Tags         Links
// @Accept       json
// @Produce      json
// @Param        body  body      CreateLinkRequest  true  "Link to create"
// @Success      201   {object}  LinkResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      401   {object}  ErrorResponse
// @Failure      409   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links [post]
func (h *linksAPIHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	var req CreateLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", CodeBadRequest)
		return
	}

	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required", CodeBadRequest)
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required", CodeBadRequest)
		return
	}

	// Validate slug format and reserved prefixes.
	// Governing: SPEC-0005 REQ "Links Collection" — slug format [a-z0-9][a-z0-9\-]*[a-z0-9]
	if err := store.ValidateSlugFormat(req.Slug); err != nil {
		if errors.Is(err, store.ErrSlugReserved) {
			writeError(w, http.StatusBadRequest, err.Error(), CodeInvalidSlug)
			return
		}
		writeError(w, http.StatusBadRequest, "slug must match [a-z0-9][a-z0-9-]*[a-z0-9]", CodeInvalidSlug)
		return
	}

	// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
	if err := store.ValidateURLVariables(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), CodeInvalidURL)
		return
	}

	// Governing: SPEC-0002 REQ "Links Table" — title max 200, description max 2000 characters
	if err := store.ValidateLinkText(req.Title, req.Description); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), CodeInvalidFieldLength)
		return
	}

	// Governing: SPEC-0010 REQ "REST API Visibility Field" — defaults to "public"
	visibility := req.Visibility
	if visibility == "" {
		visibility = "public"
	}
	if err := store.ValidateVisibility(visibility); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), CodeInvalidVisibility)
		return
	}

	// Create the link and its tags in a single transaction: if the tag write
	// fails, no link row exists, so the client's retry cannot 409 on a
	// half-created slug (issue #198).
	// Governing: SPEC-0005 REQ "Links Collection" — link+tags create atomically; ADR-0018
	link, err := h.links.CreateFull(r.Context(), req.Slug, req.URL, user.ID, req.Title, req.Description, visibility, req.Tags, nil, "")
	if err != nil {
		if errors.Is(err, store.ErrSlugTaken) {
			writeError(w, http.StatusConflict, "slug already exists", CodeSlugConflict)
			return
		}
		log.Printf("api: create link %q: %v", req.Slug, err)
		if isDBLockError(err) {
			writeError(w, http.StatusServiceUnavailable, "server is busy, please retry", CodeDBBusy)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	lr, err := h.toLinkResponse(r.Context(), link)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	writeJSON(w, http.StatusCreated, lr)
}

// Get returns a single link by ID. Owners, share recipients, and admins.
// GET /api/v1/links/{id}
// Governing: SPEC-0005 REQ "Link Resource"
// Governing: SPEC-0010 REQ "REST API Visibility Field"
//
// @Summary      Get a link
// @Description  Returns a single link by ID. Owners, share recipients, and admins may access.
// @Tags         Links
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Link ID"
// @Success      200  {object}  LinkResponse
// @Failure      401  {object}  ErrorResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links/{id} [get]
func (h *linksAPIHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	linkID := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	// Governing: SPEC-0010 REQ "REST API Visibility Field" — owners, shared users, and admins may access
	caps, err := store.LinkCapsFor(r.Context(), h.ownership, h.links, link.ID, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}
	if !caps.CanView {
		writeError(w, http.StatusForbidden, "forbidden", CodeForbidden)
		return
	}

	lr, err := h.toLinkResponse(r.Context(), link)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	writeJSON(w, http.StatusOK, lr)
}

// Update modifies a link's url, title, description, and tags. Slug is immutable and ignored.
// PUT /api/v1/links/{id}
// Governing: SPEC-0005 REQ "Link Resource" — slug field MUST be ignored (immutable)
//
// @Summary      Update a link
// @Description  Updates url, title, description, and tags. Slug is immutable and ignored.
// @Tags         Links
// @Accept       json
// @Produce      json
// @Param        id    path      string             true  "Link ID"
// @Param        body  body      UpdateLinkRequest  true  "Fields to update"
// @Success      200   {object}  LinkResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      401   {object}  ErrorResponse
// @Failure      403   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links/{id} [put]
func (h *linksAPIHandler) Update(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	linkID := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	if !requireOwnerOrAdmin(w, h.ownership, user, link.ID) {
		return
	}

	var req UpdateLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", CodeBadRequest)
		return
	}

	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required", CodeBadRequest)
		return
	}

	// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
	if err := store.ValidateURLVariables(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), CodeInvalidURL)
		return
	}

	// Governing: SPEC-0002 REQ "Links Table" — title max 200, description max 2000 characters
	if err := store.ValidateLinkText(req.Title, req.Description); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), CodeInvalidFieldLength)
		return
	}

	// Governing: SPEC-0010 REQ "REST API Visibility Field"
	visibility := link.Visibility
	if req.Visibility != "" {
		if err := store.ValidateVisibility(req.Visibility); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), CodeInvalidVisibility)
			return
		}
		visibility = req.Visibility
	}

	updated, err := h.links.Update(r.Context(), link.ID, req.URL, req.Title, req.Description, visibility)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	// Update tags. Duplicate spellings are deduped by slug inside SetTags; any
	// remaining failure is reported clearly — the link row itself was already
	// updated and PUT is idempotent, so the client can safely retry (issue #198).
	if err := h.links.SetTags(r.Context(), link.ID, req.Tags); err != nil {
		log.Printf("api: set tags for link %s: %v", link.ID, err)
		if isDBLockError(err) {
			writeError(w, http.StatusServiceUnavailable, "server is busy, please retry", CodeDBBusy)
			return
		}
		writeError(w, http.StatusInternalServerError, "link updated but tags could not be saved; retry the request", CodeTagWriteFailed)
		return
	}

	lr, err := h.toLinkResponse(r.Context(), updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	writeJSON(w, http.StatusOK, lr)
}

// Delete removes a link. Owners and admins only.
// DELETE /api/v1/links/{id}
// Governing: SPEC-0005 REQ "Link Resource"
//
// @Summary      Delete a link
// @Description  Deletes a link by ID. Only owners and admins may delete.
// @Tags         Links
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Link ID"
// @Success      204  "No Content"
// @Failure      401  {object}  ErrorResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links/{id} [delete]
func (h *linksAPIHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	linkID := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	if !requireOwnerOrAdmin(w, h.ownership, user, link.ID) {
		return
	}

	if err := h.links.Delete(r.Context(), link.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ListOwners returns all owners of a link.
// GET /api/v1/links/{id}/owners
// Governing: SPEC-0005 REQ "Co-Owner Management"
//
// @Summary      List link owners
// @Description  Returns all owners of a link. Only owners and admins may access.
// @Tags         Owners
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Link ID"
// @Success      200  {array}   OwnerResponse
// @Failure      401  {object}  ErrorResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links/{id}/owners [get]
func (h *linksAPIHandler) ListOwners(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	linkID := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	if !requireOwnerOrAdmin(w, h.ownership, user, link.ID) {
		return
	}

	owners, err := h.ownership.ListOwnerUsers(link.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	resp := make([]OwnerResponse, 0, len(owners))
	for _, o := range owners {
		resp = append(resp, OwnerResponse{
			ID:        o.ID,
			Email:     o.Email,
			IsPrimary: o.IsPrimary,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// AddOwner adds a co-owner to a link by email. Owners and admins only.
// POST /api/v1/links/{id}/owners
// Governing: SPEC-0005 REQ "Co-Owner Management"
//
// @Summary      Add a co-owner
// @Description  Adds a co-owner to a link by email address. Only owners and admins may add.
// @Tags         Owners
// @Accept       json
// @Produce      json
// @Param        id    path      string           true  "Link ID"
// @Param        body  body      AddOwnerRequest  true  "Co-owner to add"
// @Success      201   {object}  OwnerResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      401   {object}  ErrorResponse
// @Failure      403   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      409   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links/{id}/owners [post]
func (h *linksAPIHandler) AddOwner(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	linkID := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	if !requireOwnerOrAdmin(w, h.ownership, user, link.ID) {
		return
	}

	var req AddOwnerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", CodeBadRequest)
		return
	}
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required", CodeBadRequest)
		return
	}

	targetUser, err := h.users.GetByEmail(r.Context(), req.Email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	if err := h.links.AddOwner(r.Context(), link.ID, targetUser.ID); err != nil {
		if errors.Is(err, store.ErrDuplicateOwner) {
			writeError(w, http.StatusConflict, "user is already an owner", CodeDuplicateOwner)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	writeJSON(w, http.StatusCreated, OwnerResponse{
		ID:        targetUser.ID,
		Email:     targetUser.Email,
		IsPrimary: false,
	})
}

// RemoveOwner removes a co-owner from a link. Primary owner cannot be removed.
// DELETE /api/v1/links/{id}/owners/{uid}
// Governing: SPEC-0005 REQ "Co-Owner Management" — primary owner MUST be protected
//
// @Summary      Remove a co-owner
// @Description  Removes a co-owner from a link. The primary owner cannot be removed.
// @Tags         Owners
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Link ID"
// @Param        uid  path      string  true  "User ID of the owner to remove"
// @Success      204  "No Content"
// @Failure      400  {object}  ErrorResponse
// @Failure      401  {object}  ErrorResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /links/{id}/owners/{uid} [delete]
func (h *linksAPIHandler) RemoveOwner(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
		return
	}

	linkID := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	if !requireOwnerOrAdmin(w, h.ownership, user, link.ID) {
		return
	}

	ownerUID := chi.URLParam(r, "uid")
	if err := h.links.RemoveOwner(r.Context(), link.ID, ownerUID); err != nil {
		if errors.Is(err, store.ErrPrimaryOwnerImmutable) {
			writeError(w, http.StatusBadRequest, "primary owner cannot be removed", CodePrimaryOwnerProtected)
			return
		}
		if errors.Is(err, store.ErrNotOwner) {
			writeError(w, http.StatusNotFound, "owner not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// toLinkResponse converts a store.Link to an API LinkResponse, including owners and tags.
func (h *linksAPIHandler) toLinkResponse(ctx context.Context, link *store.Link) (*LinkResponse, error) {
	return buildLinkResponse(ctx, h.links, h.ownership, link)
}

// buildLinkResponse converts a store.Link to an API LinkResponse, populating
// owners, tags, and visibility. Shared by every endpoint that returns links so
// the JSON shape stays consistent.
// Governing: SPEC-0005 REQ "API Response Structures"
func buildLinkResponse(ctx context.Context, links *store.LinkStore, ownership *store.OwnershipStore, link *store.Link) (*LinkResponse, error) {
	owners, err := ownership.ListOwnerUsers(link.ID)
	if err != nil {
		return nil, err
	}

	ownerResponses := make([]OwnerResponse, 0, len(owners))
	for _, o := range owners {
		ownerResponses = append(ownerResponses, OwnerResponse{
			ID:        o.ID,
			Email:     o.Email,
			IsPrimary: o.IsPrimary,
		})
	}

	tags, err := links.ListTags(ctx, link.ID)
	if err != nil {
		return nil, err
	}
	tagNames := make([]string, 0, len(tags))
	for _, t := range tags {
		tagNames = append(tagNames, t.Name)
	}

	return &LinkResponse{
		ID:          link.ID,
		Slug:        link.Slug,
		URL:         link.URL,
		Title:       link.Title,
		Description: link.Description,
		Visibility:  link.Visibility,
		Tags:        tagNames,
		Owners:      ownerResponses,
		CreatedAt:   link.CreatedAt,
		UpdatedAt:   link.UpdatedAt,
	}, nil
}
