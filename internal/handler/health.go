// Destination-health display helpers for the web UI: derive per-row health
// states for list surfaces so the shared link_row partial can render the
// "broken" badge, gated on the viewer's capabilities, plus the admin health
// report at /admin/link-health.
//
// Governing: SPEC-0020 REQ "Health Badges and Admin Report", ADR-0020
package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// buildHealthStates derives the surfaced health state per link for badge
// rendering. Health information is shown only to viewers holding capabilities
// on the link (owners, co-owners, admins, share recipients): rows the viewer
// holds no capabilities on are absent from the map, so no badge renders for
// them. Pass rowCaps == nil for surfaces where the viewer holds capabilities
// on every row by construction (the admin views). Derivation — including the
// surfacing rule that archived/expired/opted-out links report unchecked — is
// the store's, shared with REST and MCP.
// Governing: SPEC-0020 REQ "Health Badges and Admin Report" scenario "Broken
// Badge on Owner Dashboard"
func buildHealthStates(ctx context.Context, ls *store.LinkStore, links []*store.Link, rowCaps map[string]store.LinkCaps, now time.Time) (map[string]string, error) {
	ids := make([]string, 0, len(links))
	for _, l := range links {
		if rowCaps != nil && !rowCaps[l.ID].CanView {
			continue
		}
		ids = append(ids, l.ID)
	}
	rows, err := ls.HealthForLinks(ctx, ids)
	if err != nil {
		return nil, err
	}
	states := make(map[string]string, len(ids))
	for _, l := range links {
		if rowCaps != nil && !rowCaps[l.ID].CanView {
			continue
		}
		states[l.ID] = store.DeriveHealth(l, rows[l.ID], now).Status
	}
	return states, nil
}

// AdminLinkHealthPage is the template data for the admin health report.
// Governing: SPEC-0020 REQ "Health Badges and Admin Report"
type AdminLinkHealthPage struct {
	BasePage
	Rows []*store.BrokenLink
}

// LinkHealth renders the admin report of failing links at /admin/link-health:
// all currently broken links (consecutive_failures >= 3, still eligible for
// checking) ordered by most failures first, each row linking to the link's
// detail page. Healthy, opted-out, skipped, and never-checked links are
// excluded in the store query. Access is admin-only, enforced by the router's
// RequireRole("admin") group (SPEC-0011 conventions).
// Governing: SPEC-0020 REQ "Health Badges and Admin Report" scenario "Admin
// Report Lists Failing Links"
func (h *AdminHandler) LinkHealth(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	rows, err := h.links.ListBrokenLinks(r.Context(), time.Now().UTC())
	if err != nil {
		http.Error(w, "could not load link health", http.StatusInternalServerError)
		return
	}
	data := AdminLinkHealthPage{
		BasePage: newBasePage(r, user),
		Rows:     rows,
	}
	render(w, "admin/link_health.html", data)
}
