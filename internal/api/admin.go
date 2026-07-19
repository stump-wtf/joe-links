// Governing: SPEC-0005 REQ "Admin Endpoints", ADR-0008
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// adminAPIHandler provides REST handlers for admin-only endpoints.
// Governing: SPEC-0005 REQ "Admin Endpoints"
type adminAPIHandler struct {
	users     *store.UserStore
	links     *store.LinkStore
	ownership *store.OwnershipStore
}

// registerAdminRoutes registers admin routes inside a chi Group with role-check middleware.
// Governing: SPEC-0005 REQ "Admin Endpoints" — chi Group MUST enforce role = admin.
func registerAdminRoutes(r chi.Router, users *store.UserStore, links *store.LinkStore, ownership *store.OwnershipStore) {
	h := &adminAPIHandler{users: users, links: links, ownership: ownership}

	r.Route("/admin", func(admin chi.Router) {
		// Governing: SPEC-0005 REQ "Admin Endpoints" — non-admin returns 403 Forbidden.
		admin.Use(requireAdmin)

		admin.Get("/users", h.ListUsers)
		admin.Put("/users/{id}/role", h.UpdateRole)
		admin.Get("/links", h.ListLinks)
	})
}

// requireAdmin is middleware that enforces role = admin on all routes in the group.
// Governing: SPEC-0005 REQ "Admin Endpoints" — WHEN role != admin THEN 403 Forbidden.
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		if user == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", CodeUnauthorized)
			return
		}
		if user.Role != "admin" {
			writeError(w, http.StatusForbidden, "forbidden", CodeForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ListUsers returns all users in the system.
// GET /api/v1/admin/users
// Governing: SPEC-0005 REQ "Admin Endpoints"
//
// @Summary      List all users (admin)
// @Description  Returns all users in the system. Requires admin role.
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Param        limit   query     int     false  "Max items to return (default 50, max 200)"
// @Param        cursor  query     string  false  "Opaque pagination cursor from a prior next_cursor"
// @Success      200  {object}  UserListResponse
// @Failure      401  {object}  ErrorResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /admin/users [get]
func (h *adminAPIHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	// Governing: SPEC-0005 REQ "Pagination" — ?limit (default 50, max 200) + opaque ?cursor
	limit := parseLimit(r)
	cursorName, cursorID := parseCursor(r)

	// Fetch limit+1 to detect whether another page exists.
	users, err := h.users.ListAllPaginated(r.Context(), limit+1, cursorName, cursorID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	var nextCursor *string
	if len(users) > limit {
		last := users[limit-1]
		c := encodeCursor(last.DisplayName, last.ID)
		nextCursor = &c
		users = users[:limit]
	}

	resp := &UserListResponse{Users: make([]*UserResponse, 0, len(users)), NextCursor: nextCursor}
	for _, u := range users {
		resp.Users = append(resp.Users, &UserResponse{
			ID:          u.ID,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			Role:        u.Role,
			CreatedAt:   u.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// UpdateRole changes a user's role. Accepts only "user" and "admin".
// PUT /api/v1/admin/users/{id}/role
// Governing: SPEC-0005 REQ "Admin Endpoints" — valid roles: "user", "admin".
//
// @Summary      Update user role (admin)
// @Description  Changes a user's role. Valid values: "user", "admin". Requires admin role.
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Param        id    path      string             true  "User ID"
// @Param        body  body      UpdateRoleRequest  true  "New role"
// @Success      200   {object}  UserResponse
// @Failure      400   {object}  ErrorResponse
// @Failure      401   {object}  ErrorResponse
// @Failure      403   {object}  ErrorResponse
// @Failure      404   {object}  ErrorResponse
// @Failure      500   {object}  ErrorResponse
// @Security     BearerToken
// @Router       /admin/users/{id}/role [put]
func (h *adminAPIHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "id")

	var req UpdateRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", CodeBadRequest)
		return
	}

	if req.Role != "user" && req.Role != "admin" {
		writeError(w, http.StatusBadRequest, "role must be \"user\" or \"admin\"", CodeBadRequest)
		return
	}

	updated, err := h.users.UpdateRole(r.Context(), userID, req.Role)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "user not found", CodeNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	writeJSON(w, http.StatusOK, &UserResponse{
		ID:          updated.ID,
		Email:       updated.Email,
		DisplayName: updated.DisplayName,
		Role:        updated.Role,
		CreatedAt:   updated.CreatedAt,
	})
}

// ListLinks returns all links system-wide (admin-only explicit route).
// GET /api/v1/admin/links
// Governing: SPEC-0005 REQ "Admin Endpoints"
//
// @Summary      List all links (admin)
// @Description  Returns all links system-wide with owners and tags. Requires admin role.
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Param        limit   query     int     false  "Max items to return (default 50, max 200)"
// @Param        cursor  query     string  false  "Opaque pagination cursor from a prior next_cursor"
// @Success      200  {object}  LinkListResponse
// @Failure      401  {object}  ErrorResponse
// @Failure      403  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Security     BearerToken
// @Router       /admin/links [get]
func (h *adminAPIHandler) ListLinks(w http.ResponseWriter, r *http.Request) {
	// Governing: SPEC-0005 REQ "Pagination" — ?limit (default 50, max 200) + opaque ?cursor
	limit := parseLimit(r)
	cursorSlug, cursorID := parseCursor(r)

	// Fetch limit+1 to detect whether another page exists.
	links, err := h.links.ListAllPaginated(r.Context(), limit+1, cursorSlug, cursorID)
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

	// Governing: SPEC-0005 REQ "API Response Structures" — reuse buildLinkResponse
	// so the admin list carries the same shape (owners, tags, visibility) as every
	// other link endpoint; the previous hand-built response omitted visibility.
	resp := &LinkListResponse{Links: make([]*LinkResponse, 0, len(links)), NextCursor: nextCursor}
	for _, l := range links {
		// Admin callers hold capabilities on every link, so the health object
		// is included on each row.
		// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP"
		lr, err := buildLinkResponse(r.Context(), h.links, h.ownership, l, true)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
			return
		}
		resp.Links = append(resp.Links, lr)
	}

	writeJSON(w, http.StatusOK, resp)
}
