// Governing: SPEC-0016 REQ "REST API Stats Endpoint", REQ "REST API Clicks Endpoint", ADR-0016, ADR-0008, ADR-0009
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

type statsAPIHandler struct {
	links  *store.LinkStore
	clicks *store.ClickStore
	owns   *store.OwnershipStore
}

func newStatsAPIHandler(ls *store.LinkStore, cs *store.ClickStore, os *store.OwnershipStore) *statsAPIHandler {
	return &statsAPIHandler{links: ls, clicks: cs, owns: os}
}

// statsResponse is the JSON shape for GET /api/v1/links/{id}/stats.
type statsResponse struct {
	LinkID  string `json:"link_id"`
	Total   int64  `json:"total"`
	Last7d  int64  `json:"last_7d"`
	Last30d int64  `json:"last_30d"`
}

// clickResponse is one entry in the clicks list.
type clickResponse struct {
	ClickedAt time.Time     `json:"clicked_at"`
	Referrer  *string       `json:"referrer"`
	User      *clickUserRef `json:"user"`
}

type clickUserRef struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// clickListResponse is the JSON shape for GET /api/v1/links/{id}/clicks.
type clickListResponse struct {
	Clicks     []clickResponse `json:"clicks"`
	NextCursor *string         `json:"next_cursor"`
}

// GetStats returns aggregate click stats for a link.
// GET /api/v1/links/{id}/stats
// Governing: SPEC-0016 REQ "REST API Stats Endpoint", ADR-0016
func (h *statsAPIHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "UNAUTHORIZED")
		return
	}

	linkID := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found", "NOT_FOUND")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL_ERROR")
		return
	}

	// Owners/co-owners/admins and share recipients may read stats.
	// Governing: SPEC-0016 REQ "REST API Stats Endpoint"
	// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL_ERROR")
		return
	}
	if !caps.CanStats {
		writeError(w, http.StatusForbidden, "forbidden", "FORBIDDEN")
		return
	}

	stats, err := h.clicks.GetClickStats(r.Context(), link.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL_ERROR")
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		LinkID:  link.ID,
		Total:   stats.Total,
		Last7d:  stats.Last7d,
		Last30d: stats.Last30d,
	})
}

// ListClicks returns paginated click events for a link.
// GET /api/v1/links/{id}/clicks
// Governing: SPEC-0016 REQ "REST API Clicks Endpoint", ADR-0016
func (h *statsAPIHandler) ListClicks(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "UNAUTHORIZED")
		return
	}

	linkID := chi.URLParam(r, "id")
	link, err := h.links.GetByID(r.Context(), linkID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found", "NOT_FOUND")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL_ERROR")
		return
	}

	// Same read matrix as GetStats: recipients may read clicks too.
	// Governing: SPEC-0016 REQ "REST API Clicks Endpoint"
	// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL_ERROR")
		return
	}
	if !caps.CanStats {
		writeError(w, http.StatusForbidden, "forbidden", "FORBIDDEN")
		return
	}

	// Parse limit (default 50, max 200).
	limit := parseLimit(r)

	// Parse the before cursor. Accepts the opaque (clicked_at, id) keyset
	// cursor issued in next_cursor, or — for backward compatibility with
	// cursors issued before the ID tiebreaker existed — a bare ISO 8601 /
	// RFC 3339 timestamp. Anything else is a 400.
	// Governing: SPEC-0016 REQ "REST API Clicks Endpoint", SPEC-0005 REQ "Pagination"
	var before time.Time
	var beforeID string
	if v := r.URL.Query().Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			// Legacy timestamp-only cursor.
			before = t
		} else if ts, id := decodeCursor(v); ts != "" && id != "" {
			t, err := time.Parse(time.RFC3339Nano, ts)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid before cursor", "BAD_REQUEST")
				return
			}
			before = t
			beforeID = id
		} else {
			writeError(w, http.StatusBadRequest, "invalid before cursor, expected next_cursor value or RFC 3339 timestamp", "BAD_REQUEST")
			return
		}
	}

	// Fetch limit+1 to detect next page.
	rows, err := h.clicks.ListRecentClicksBefore(r.Context(), link.ID, before, beforeID, limit+1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL_ERROR")
		return
	}

	var nextCursor *string
	if len(rows) > limit {
		last := rows[limit-1]
		cursor := encodeCursor(last.ClickedAt.Format(time.RFC3339Nano), last.ID)
		nextCursor = &cursor
		rows = rows[:limit]
	}

	clicks := make([]clickResponse, 0, len(rows))
	for _, rc := range rows {
		cr := clickResponse{
			ClickedAt: rc.ClickedAt,
		}
		if rc.Referrer != "" {
			ref := rc.Referrer
			cr.Referrer = &ref
		}
		if rc.UserID != "" {
			cr.User = &clickUserRef{
				ID:          rc.UserID,
				DisplayName: rc.DisplayName,
			}
		}
		clicks = append(clicks, cr)
	}

	writeJSON(w, http.StatusOK, clickListResponse{
		Clicks:     clicks,
		NextCursor: nextCursor,
	})
}
