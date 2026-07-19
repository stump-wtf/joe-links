// Governing: SPEC-0001 REQ "Short Link Management", REQ "HTMX Hypermedia Interactions", ADR-0001
// Governing: SPEC-0003 REQ "Theme Persistence via Cookie", ADR-0006
// Governing: SPEC-0004 REQ "New Link Form", "Edit Link Form", "Delete Link"
// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
package handler

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// LinkForm holds form input values for creating or editing a link.
// Governing: SPEC-0010 REQ "Visibility Selector in Link Forms"
// Governing: SPEC-0020 REQ "Link Expiration" — optional expiration input, empty by default
type LinkForm struct {
	Slug        string
	URL         string
	Title       string
	Description string
	Tags        string // comma-separated tag names
	Visibility  string // public, private, or secure
	ExpiresAt   string // datetime-local value (UTC), empty = never expires
}

// LinkFormPage is the template data for the new/edit link forms.
type LinkFormPage struct {
	BasePage
	User  *store.User
	Link  *store.Link
	Form  LinkForm
	Error string
	Flash *Flash
}

// LinkDetailPage is the template data for the link detail view.
// Governing: SPEC-0004 REQ "Link Detail View"
// Governing: SPEC-0010 REQ "Share Management Panel on Link Detail"
type LinkDetailPage struct {
	BasePage
	User   *store.User
	Link   *store.Link
	Tags   []*store.Tag
	Owners []*store.OwnerInfo
	Shares []ShareUser
	Error  string
	// Capability gates: share recipients view the page read-only, so every
	// mutating control renders only for owners/co-owners/admins.
	// Governing: SPEC-0010 REQ "Link Shares Table" — recipients are read-only
	CanEdit   bool // Edit button
	CanDelete bool // Delete button + confirm modal
	CanManage bool // owner add/remove controls and the shares panel
}

// ShareUser combines share record with user display info for templates.
// Governing: SPEC-0010 REQ "Share Management Panel on Link Detail"
type ShareUser struct {
	UserID      string
	DisplayName string
	Email       string
}

// ConfirmDeleteData holds template data for the delete confirmation modal.
// Governing: SPEC-0013 REQ "DaisyUI Delete Confirmation Modal"
type ConfirmDeleteData struct {
	Name      string
	DeleteURL string
	Target    string
}

// LinksHandler provides HTTP handlers for link CRUD operations.
type LinksHandler struct {
	links    *store.LinkStore
	owns     *store.OwnershipStore
	users    *store.UserStore
	keywords *store.KeywordStore
}

// NewLinksHandler creates a new LinksHandler.
func NewLinksHandler(ls *store.LinkStore, os *store.OwnershipStore, us *store.UserStore, ks *store.KeywordStore) *LinksHandler {
	return &LinksHandler{links: ls, owns: os, users: us, keywords: ks}
}

// New renders the create-link form.
// Governing: SPEC-0004 REQ "New Link Form"
func (h *LinksHandler) New(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	form := LinkForm{Slug: r.URL.Query().Get("slug")}

	data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form}
	if isHTMX(r) {
		renderFragment(w, "new_link_modal", data)
		return
	}
	render(w, "new.html", data)
}

// Create processes the create-link form submission.
// Governing: SPEC-0004 REQ "New Link Form"
func (h *LinksHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Governing: SPEC-0010 REQ "Visibility Selector in Link Forms"
	visibility := r.FormValue("visibility")
	if visibility == "" {
		visibility = "public"
	}

	form := LinkForm{
		Slug:        r.FormValue("slug"),
		URL:         r.FormValue("url"),
		Title:       r.FormValue("title"),
		Description: r.FormValue("description"),
		Tags:        r.FormValue("tags"),
		Visibility:  visibility,
		ExpiresAt:   r.FormValue("expires_at"),
	}

	// Optional expiration: empty means never expires; a past value is rejected
	// before any row is written.
	// Governing: SPEC-0020 REQ "Link Expiration" scenarios "Link Created with
	// Expiration", "Past Expiration Rejected"
	expiresAt, expErr := parseExpiresAtForm(form.ExpiresAt)
	if expErr == nil {
		expErr = store.ValidateExpiresAt(expiresAt, nil, time.Now().UTC())
	}
	if expErr != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form, Error: expErr.Error()}
		if isHTMX(r) {
			renderFragment(w, "new_link_modal", data)
			return
		}
		render(w, "new.html", data)
		return
	}

	// Governing: SPEC-0010 REQ "Visibility Selector in Link Forms" — validate visibility value
	if err := store.ValidateVisibility(form.Visibility); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "new_link_modal", data)
			return
		}
		render(w, "new.html", data)
		return
	}

	// Slug format and reserved-word checks share the store's single source of
	// truth (store.ValidateSlugFormat) with the REST API, MCP tools, and the
	// live availability checker; the reserved error names the full set (#204).
	// Governing: SPEC-0001 REQ "Short Link Resolution" — reserved routes MUST NOT be valid slugs
	// Governing: SPEC-0013 REQ "Create/Edit Link Form as HTMX Modal" — validation errors re-render inside modal
	if err := store.ValidateSlugFormat(form.Slug); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "new_link_modal", data)
			return
		}
		render(w, "new.html", data)
		return
	}

	// Scheme allowlist: only http(s) destinations may be stored (issue #265).
	if err := store.ValidateLinkURL(form.URL); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "new_link_modal", data)
			return
		}
		render(w, "new.html", data)
		return
	}

	// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
	if err := store.ValidateURLVariables(form.URL); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "new_link_modal", data)
			return
		}
		render(w, "new.html", data)
		return
	}

	// Governing: SPEC-0002 REQ "Links Table" — title max 200, description max 2000 characters
	if err := store.ValidateLinkText(form.Title, form.Description); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "new_link_modal", data)
			return
		}
		render(w, "new.html", data)
		return
	}

	// Tag intake validation: safe charset, bounded length and count, shared
	// with the REST API and MCP tools via the store validators (issues #251, #265).
	tagNames := parseTagNames(form.Tags)
	if err := store.ValidateTagNames(tagNames); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "new_link_modal", data)
			return
		}
		render(w, "new.html", data)
		return
	}

	// Create the link and its tags in a single transaction: a failed tag write
	// rolls back the link too, so it re-renders the form with an error instead
	// of silently dropping the tags (issue #198).
	// Governing: SPEC-0004 REQ "New Link Form" — link+tags create atomically; ADR-0018
	_, err := h.links.CreateFull(r.Context(), form.Slug, form.URL, user.ID, form.Title, form.Description, form.Visibility, expiresAt, tagNames, nil, "")
	if err != nil {
		msg := "Could not create the link. Please try again."
		if errors.Is(err, store.ErrSlugTaken) {
			msg = "That slug is already taken. Choose a different one."
		} else if errors.Is(err, store.ErrExpiresAtInPast) {
			// The store re-validates expires_at with a later clock than the
			// handler check above; surface the validation message, not a
			// generic failure.
			// Governing: SPEC-0020 REQ "Link Expiration" scenario "Past Expiration Rejected"
			msg = err.Error()
		} else {
			log.Printf("create link %q: %v", form.Slug, err)
		}
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Form: form, Error: msg}
		if isHTMX(r) {
			renderFragment(w, "new_link_modal", data)
			return
		}
		render(w, "new.html", data)
		return
	}

	// Governing: SPEC-0013 REQ "Create/Edit Link Form as HTMX Modal" — close modal + trigger list refresh
	if isHTMX(r) {
		w.Header().Set("HX-Trigger", "linkCreated")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// Edit renders the edit-link form.
// Governing: SPEC-0004 REQ "Edit Link Form"
// Governing: SPEC-0013 REQ "Create/Edit Link Form as HTMX Modal"
func (h *LinksHandler) Edit(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	id := chi.URLParam(r, "id")

	link, err := h.links.GetByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
	allowed, err := store.IsOwnerOrAdmin(h.owns, link.ID, user.ID, user.Role)
	if err != nil {
		log.Printf("ownership check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		RenderForbidden(w, r)
		return
	}

	// Load current tags for pre-fill
	tags, _ := h.links.ListTags(r.Context(), link.ID)
	tagNames := make([]string, len(tags))
	for i, t := range tags {
		tagNames[i] = t.Name
	}

	form := LinkForm{
		URL:         link.URL,
		Title:       link.Title,
		Description: link.Description,
		Tags:        strings.Join(tagNames, ", "),
		Visibility:  link.Visibility,
		// Pre-fill the stored expiration so it round-trips unchanged (expired
		// links stay editable) and is clearable on edit.
		// Governing: SPEC-0020 REQ "Link Expiration"
		ExpiresAt: formatExpiresAtForm(link.ExpiresAt),
	}

	data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form}
	if isHTMX(r) {
		renderFragment(w, "edit_link_modal", data)
		return
	}
	render(w, "edit.html", data)
}

// Update processes the edit-link form submission.
// Governing: SPEC-0004 REQ "Edit Link Form"
func (h *LinksHandler) Update(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	id := chi.URLParam(r, "id")

	link, err := h.links.GetByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
	allowed, err := store.IsOwnerOrAdmin(h.owns, link.ID, user.ID, user.Role)
	if err != nil {
		log.Printf("ownership check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		RenderForbidden(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Governing: SPEC-0001 REQ "Short Link Management" — slug is immutable after creation.
	// Governing: SPEC-0010 REQ "Visibility Selector in Link Forms"
	visibility := r.FormValue("visibility")
	if visibility == "" {
		visibility = link.Visibility
	}

	form := LinkForm{
		URL:         r.FormValue("url"),
		Title:       r.FormValue("title"),
		Description: r.FormValue("description"),
		Tags:        r.FormValue("tags"),
		Visibility:  visibility,
		ExpiresAt:   r.FormValue("expires_at"),
	}

	// Expiration: the edit form always submits the field — empty clears it,
	// the round-tripped stored (possibly past) value is accepted unchanged,
	// and a NEW past value is rejected. Share recipients never reach this
	// handler (IsOwnerOrAdmin gate above).
	// Governing: SPEC-0020 REQ "Link Expiration" scenarios "Past Expiration
	// Rejected", "Expired Link Stays Editable", "Expiration Cleared on Edit",
	// "Share Recipient Cannot Set Expiry"
	expiresAt, expErr := parseExpiresAtForm(form.ExpiresAt)
	if expErr == nil {
		expErr = store.ValidateExpiresAt(expiresAt, link.ExpiresAt, time.Now().UTC())
	}
	if expErr != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: expErr.Error()}
		if isHTMX(r) {
			renderFragment(w, "edit_link_modal", data)
			return
		}
		render(w, "edit.html", data)
		return
	}

	// Governing: SPEC-0010 REQ "Visibility Selector in Link Forms" — validate visibility value
	if err := store.ValidateVisibility(form.Visibility); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "edit_link_modal", data)
			return
		}
		render(w, "edit.html", data)
		return
	}

	// Scheme allowlist: only http(s) destinations may be stored (issue #265).
	if err := store.ValidateLinkURL(form.URL); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "edit_link_modal", data)
			return
		}
		render(w, "edit.html", data)
		return
	}

	// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
	if err := store.ValidateURLVariables(form.URL); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "edit_link_modal", data)
			return
		}
		render(w, "edit.html", data)
		return
	}

	// Governing: SPEC-0002 REQ "Links Table" — title max 200, description max 2000 characters
	if err := store.ValidateLinkText(form.Title, form.Description); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "edit_link_modal", data)
			return
		}
		render(w, "edit.html", data)
		return
	}

	// Tag intake validation before any write, so a hostile tag name cannot
	// leave the link row updated with its tag update rejected (issues #251, #265).
	tagNames := parseTagNames(form.Tags)
	if err := store.ValidateTagNames(tagNames); err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: err.Error()}
		if isHTMX(r) {
			renderFragment(w, "edit_link_modal", data)
			return
		}
		render(w, "edit.html", data)
		return
	}

	_, err = h.links.Update(r.Context(), id, form.URL, form.Title, form.Description, form.Visibility, expiresAt)
	if err != nil {
		msg := "Update failed."
		// The store re-validates expires_at with a later clock than the handler
		// check above; surface the validation message, not a generic failure.
		// Governing: SPEC-0020 REQ "Link Expiration" scenario "Past Expiration Rejected"
		if errors.Is(err, store.ErrExpiresAtInPast) {
			msg = err.Error()
		}
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: msg}
		// Governing: SPEC-0013 REQ "Create/Edit Link Form as HTMX Modal" — re-render inside modal on error
		if isHTMX(r) {
			renderFragment(w, "edit_link_modal", data)
			return
		}
		render(w, "edit.html", data)
		return
	}

	// Update tags. A failed tag write must surface to the user, not be
	// silently discarded (issue #198).
	// Governing: SPEC-0004 REQ "Edit Link Form"
	if err := h.links.SetTags(r.Context(), id, tagNames); err != nil {
		log.Printf("set tags for link %s: %v", id, err)
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: "The link was saved, but its tags could not be updated. Please try again."}
		// Governing: SPEC-0013 REQ "Create/Edit Link Form as HTMX Modal" — re-render inside modal on error
		if isHTMX(r) {
			renderFragment(w, "edit_link_modal", data)
			return
		}
		render(w, "edit.html", data)
		return
	}

	// Governing: SPEC-0013 REQ "Create/Edit Link Form as HTMX Modal" — close modal + trigger list refresh
	if isHTMX(r) {
		w.Header().Set("HX-Trigger", "linkUpdated")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/dashboard/links/"+id, http.StatusSeeOther)
}

// Delete removes a link. Returns 200 with empty body for HTMX row removal.
// Governing: SPEC-0004 REQ "Delete Link"
func (h *LinksHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	id := chi.URLParam(r, "id")

	link, err := h.links.GetByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
	allowed, err := store.IsOwnerOrAdmin(h.owns, link.ID, user.ID, user.Role)
	if err != nil {
		log.Printf("ownership check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		RenderForbidden(w, r)
		return
	}

	if err := h.links.Delete(r.Context(), id); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	// Governing: SPEC-0004 REQ "Delete Link" — OOB toast on success
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<div id="toast-area" hx-swap-oob="innerHTML:#toast-area"><div class="alert alert-success"><span>Link deleted.</span></div></div>`))
}

// Archive handles POST /dashboard/links/{id}/archive: a reversible off switch
// distinct from delete — the row, slug reservation, and click history all
// survive.
// Governing: SPEC-0020 REQ "Archive State" scenario "Archive Toggle Stops
// Resolution, Keeps Stats"
func (h *LinksHandler) Archive(w http.ResponseWriter, r *http.Request) {
	h.setArchived(w, r, true)
}

// Unarchive handles POST /dashboard/links/{id}/unarchive, clearing
// archived_at so the slug resolves again (absent expiry).
// Governing: SPEC-0020 REQ "Archive State" scenario "Unarchive Restores Resolution"
func (h *LinksHandler) Unarchive(w http.ResponseWriter, r *http.Request) {
	h.setArchived(w, r, false)
}

// setArchived is the shared archive/unarchive toggle. The state change itself
// lives in the store (SetArchived) so web, REST, and MCP cannot diverge.
// Governing: SPEC-0020 REQ "Archive State", ADR-0020
func (h *LinksHandler) setArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	user := auth.UserFromContext(r.Context())
	id := chi.URLParam(r, "id")

	link, err := h.links.GetByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Archive and unarchive are edits: owners, co-owners, and admins only.
	// Share recipients and unrelated users get 403.
	// Governing: SPEC-0020 REQ "Archive State" scenario "Non-Editor Cannot Archive"
	allowed, err := store.IsOwnerOrAdmin(h.owns, link.ID, user.ID, user.Role)
	if err != nil {
		log.Printf("ownership check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		RenderForbidden(w, r)
		return
	}

	if _, err := h.links.SetArchived(r.Context(), link.ID, archived); err != nil {
		log.Printf("set archived=%v for link %s: %v", archived, link.ID, err)
		http.Error(w, "could not update archive state", http.StatusInternalServerError)
		return
	}

	// HTMX-aware: swap the refreshed detail view in place; plain requests
	// redirect back to the detail page.
	// Governing: SPEC-0020 REQ "Archive State" — toggle on the detail surface, HTMX-aware
	if isHTMX(r) {
		h.Detail(w, r)
		return
	}
	http.Redirect(w, r, "/dashboard/links/"+link.ID, http.StatusSeeOther)
}

// Renew handles POST /dashboard/links/{id}/renew: one-click clearing of
// expires_at so an expired link resolves again. Renew never touches
// archived_at — an archived link stays archived (and keeps not resolving).
// Governing: SPEC-0020 REQ "Renewal"
func (h *LinksHandler) Renew(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	id := chi.URLParam(r, "id")

	link, err := h.links.GetByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Renewal is an edit; share recipients see the expired badge but cannot
	// renew.
	// Governing: SPEC-0020 REQ "Renewal" scenario "Renew Requires Edit Capability"
	caps, err := store.LinkCapsFor(r.Context(), h.owns, h.links, link.ID, user)
	if err != nil {
		log.Printf("capability check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !caps.CanEdit {
		RenderForbidden(w, r)
		return
	}

	renewed, err := h.links.Renew(r.Context(), link.ID)
	if err != nil {
		log.Printf("renew link %s: %v", link.ID, err)
		http.Error(w, "could not renew link", http.StatusInternalServerError)
		return
	}

	// HTMX swaps the affected row back in without the expired badge or renew
	// button; plain requests fall back to the dashboard. The fragment must
	// reproduce the originating surface's column set — the renew button
	// renders on both the dashboard (no Title column) and the tag detail page
	// (Title column present), and a row swapped in with the wrong shape would
	// misalign every cell after the missing one.
	// Governing: SPEC-0020 REQ "Renewal" scenario "One-Click Renew Clears Expiry"
	if isHTMX(r) {
		ctxData := DashboardPage{
			BasePage:       newBasePage(r, user),
			User:           user,
			ShowTitle:      renewSourceShowsTitle(r),
			ShowVisibility: true,
			ShowActions:    true,
			ShowLifecycle:  true,
			RowCaps:        map[string]store.LinkCaps{link.ID: caps},
		}
		renderFragment(w, "link_row", map[string]any{"Link": renewed, "Ctx": ctxData})
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// renewSourceShowsTitle reports whether the surface the renew click came from
// renders the Title column, so the swapped-in row matches its table's shape.
// HTMX sends the page URL in HX-Current-URL; the tag detail page
// (/dashboard/tags/{slug}, internal/handler/tags.go) sets ShowTitle while the
// dashboard does not. Absent or unparseable headers fall back to the
// dashboard shape.
// Governing: SPEC-0020 REQ "Renewal" scenario "One-Click Renew Clears Expiry"
func renewSourceShowsTitle(r *http.Request) bool {
	cur, err := url.Parse(r.Header.Get("HX-Current-URL"))
	if err != nil {
		return false
	}
	return strings.HasPrefix(cur.Path, "/dashboard/tags/")
}

// ConfirmDelete renders the delete confirmation modal for a link.
// Governing: SPEC-0013 REQ "DaisyUI Delete Confirmation Modal"
func (h *LinksHandler) ConfirmDelete(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	id := chi.URLParam(r, "id")

	link, err := h.links.GetByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Governing: SPEC-0002 REQ "Authorization Based on Ownership"
	allowed, err := store.IsOwnerOrAdmin(h.owns, link.ID, user.ID, user.Role)
	if err != nil {
		log.Printf("ownership check failed for link %s user %s: %v", link.ID, user.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		RenderForbidden(w, r)
		return
	}

	data := ConfirmDeleteData{
		Name:      link.Slug,
		DeleteURL: "/dashboard/links/" + id,
		Target:    "#link-" + id,
	}
	renderFragment(w, "confirm_delete", data)
}

// expiresAtFormLayout is the wire format of the expiration form input: HTML
// datetime-local with step="1" (seconds shown so a stored value round-trips
// losslessly). Values are interpreted as UTC, matching the column semantics.
// Governing: SPEC-0020 REQ "Link Expiration"
const expiresAtFormLayout = "2006-01-02T15:04:05"

// parseExpiresAtForm parses the optional expires_at form field. Empty means
// "no expiration" (create) / "clear it" (edit). Browsers omit trailing
// zero-seconds from datetime-local values, so the minute-precision form is
// accepted too.
// Governing: SPEC-0020 REQ "Link Expiration"
func parseExpiresAtForm(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{expiresAtFormLayout, "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return &t, nil
		}
	}
	return nil, errors.New("expiration must be a date and time (UTC)")
}

// formatExpiresAtForm renders a stored expires_at for the edit form input.
func formatExpiresAtForm(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(expiresAtFormLayout)
}

// parseTagNames splits a comma-separated string into trimmed, non-empty tag names.
func parseTagNames(s string) []string {
	var names []string
	for _, part := range strings.Split(s, ",") {
		name := strings.TrimSpace(part)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
