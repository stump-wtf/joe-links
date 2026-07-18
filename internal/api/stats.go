// Governing: SPEC-0016 REQ "REST API Stats Endpoint", REQ "REST API Clicks Endpoint", ADR-0016, ADR-0008, ADR-0009
// Governing: SPEC-0021 REQ "Time Series API", ADR-0021
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

// TimeSeriesPoint is one UTC-calendar-day bucket in the timeseries response.
// Governing: SPEC-0021 REQ "Time Series API"
type TimeSeriesPoint struct {
	Date  string `json:"date"` // UTC calendar day, YYYY-MM-DD
	Count int64  `json:"count"`
}

// TimeSeriesResponse is the JSON shape for GET /api/v1/links/{id}/stats/timeseries.
// Governing: SPEC-0021 REQ "Time Series API"
type TimeSeriesResponse struct {
	LinkID string            `json:"link_id"`
	Days   int               `json:"days"`
	Series []TimeSeriesPoint `json:"series"`
}

// GetStats returns aggregate click stats for a link.
// GET /api/v1/links/{id}/stats
// Governing: SPEC-0016 REQ "REST API Stats Endpoint", ADR-0016
func (h *statsAPIHandler) GetStats(w http.ResponseWriter, r *http.Request) {
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

	// Owners/co-owners/admins and share recipients may read stats.
	// Governing: SPEC-0016 REQ "REST API Stats Endpoint"
	// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}
	if !caps.CanStats {
		writeError(w, http.StatusForbidden, "forbidden", CodeForbidden)
		return
	}

	stats, err := h.clicks.GetClickStats(r.Context(), link.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
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

	// Same read matrix as GetStats: recipients may read clicks too.
	// Governing: SPEC-0016 REQ "REST API Clicks Endpoint"
	// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}
	if !caps.CanStats {
		writeError(w, http.StatusForbidden, "forbidden", CodeForbidden)
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
				writeError(w, http.StatusBadRequest, "invalid before cursor", CodeBadRequest)
				return
			}
			before = t
			beforeID = id
		} else {
			writeError(w, http.StatusBadRequest, "invalid before cursor, expected next_cursor value or RFC 3339 timestamp", CodeBadRequest)
			return
		}
	}

	// Fetch limit+1 to detect next page.
	rows, err := h.clicks.ListRecentClicksBefore(r.Context(), link.ID, before, beforeID, limit+1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
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
		// Clicker attribution is manager-only: for a secure link the set of
		// authenticated clickers is a proxy for the share roster, which
		// recipients deliberately cannot see. See PR #255 security review.
		// Governing: SPEC-0010 REQ "Link Shares Table" — recipients get read-only access
		if rc.UserID != "" && caps.CanManageShares {
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

// GetTimeSeries returns the daily click time series for a link.
// GET /api/v1/links/{id}/stats/timeseries
//
//	@Summary		Get link click time series
//	@Description	Daily click counts for the link over the last 30 (default) or 90 UTC calendar days. The series contains exactly `days` entries, ascending by date, gap-filled with zeros.
//	@Tags			Stats
//	@Accept			json
//	@Produce		json
//	@Param			id		path	string	true	"Link ID"
//	@Param			days	query	int		false	"Window size in days: 30 (default) or 90"
//	@Success		200	{object}	TimeSeriesResponse
//	@Failure		400	{object}	ErrorResponse
//	@Failure		401	{object}	ErrorResponse
//	@Failure		403	{object}	ErrorResponse
//	@Failure		404	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerToken
//	@Router			/links/{id}/stats/timeseries [get]
//
// Governing: SPEC-0021 REQ "Time Series API", ADR-0021
func (h *statsAPIHandler) GetTimeSeries(w http.ResponseWriter, r *http.Request) {
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

	// Same read matrix as GetStats: owners, co-owners, admins, and share
	// recipients may read the series. It is counts-only — no attribution can
	// leak to recipients.
	// Governing: SPEC-0021 REQ "Time Series API", REQ "Capability Gating of Analytics Surfaces"
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}
	if !caps.CanStats {
		writeError(w, http.StatusForbidden, "forbidden", CodeForbidden)
		return
	}

	// The optional days parameter accepts exactly the literal strings "30"
	// (default) and "90"; any other value — including numeric-equivalent
	// spellings like "030" or "+30" — is a 400 in the standard error shape
	// (SPEC-0005).
	// Governing: SPEC-0021 REQ "Time Series API"
	days := 30
	switch r.URL.Query().Get("days") {
	case "", "30":
	case "90":
		days = 90
	default:
		writeError(w, http.StatusBadRequest, "invalid days parameter, expected 30 or 90", CodeBadRequest)
		return
	}

	// Retention (JOE_CLICK_RETENTION) is wired by the retention story; until
	// then the API computes with retention disabled. Either way the pinned
	// response shape carries no pruned marker — pruned days surface as
	// zero-count entries in JSON and as no-data only on the web chart.
	// Governing: SPEC-0021 REQ "Time Series API"
	series, err := h.clicks.GetDailyClickSeries(r.Context(), link.ID, days, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error", CodeInternalError)
		return
	}

	points := make([]TimeSeriesPoint, 0, len(series))
	for _, d := range series {
		points = append(points, TimeSeriesPoint{Date: d.Date, Count: d.Count})
	}

	writeJSON(w, http.StatusOK, TimeSeriesResponse{
		LinkID: link.ID,
		Days:   days,
		Series: points,
	})
}
