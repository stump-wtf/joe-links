// Governing: SPEC-0001 REQ "Short Link Management", REQ "HTMX Hypermedia Interactions", ADR-0001
// Governing: SPEC-0003 REQ "Theme Persistence via Cookie", ADR-0006
// Governing: SPEC-0004 REQ "New Link Form", "Edit Link Form", "Delete Link"
// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
)

// LinkForm holds form input values for creating or editing a link.
// Governing: SPEC-0010 REQ "Visibility Selector in Link Forms"
type LinkForm struct {
	Slug        string
	URL         string
	Title       string
	Description string
	Tags        string // comma-separated tag names
	Visibility  string // public, private, or secure
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

	// Create the link and its tags in a single transaction: a failed tag write
	// rolls back the link too, so it re-renders the form with an error instead
	// of silently dropping the tags (issue #198).
	// Governing: SPEC-0004 REQ "New Link Form" — link+tags create atomically; ADR-0018
	tagNames := parseTagNames(form.Tags)
	_, err := h.links.CreateFull(r.Context(), form.Slug, form.URL, user.ID, form.Title, form.Description, form.Visibility, tagNames, nil, "")
	if err != nil {
		msg := "Could not create the link. Please try again."
		if errors.Is(err, store.ErrSlugTaken) {
			msg = "That slug is already taken. Choose a different one."
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

	_, err = h.links.Update(r.Context(), id, form.URL, form.Title, form.Description, form.Visibility)
	if err != nil {
		data := LinkFormPage{BasePage: newBasePage(r, user), User: user, Link: link, Form: form, Error: "Update failed."}
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
	tagNames := parseTagNames(form.Tags)
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
