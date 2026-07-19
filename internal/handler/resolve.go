// Governing: SPEC-0001 REQ "Short Link Resolution", REQ "HTMX Hypermedia Interactions", ADR-0001
// Governing: SPEC-0003 REQ "Theme Persistence via Cookie", ADR-0006
// Governing: SPEC-0004 REQ "Slug Resolver and 404 Page"
// Governing: SPEC-0009 REQ "Multi-Segment Path Resolution", REQ "Variable Substitution and Redirect", ADR-0013
// Governing: SPEC-0010 REQ "Secure Link Resolution", REQ "Public Link Resolution", REQ "Private Link Resolution", ADR-0014
// Governing: SPEC-0016 REQ "Click Recording", REQ "Prometheus Metrics Endpoint", ADR-0016
// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution", REQ "Slug Normalization Forgiveness", REQ "Did-You-Mean 404 Suggestions", ADR-0019
// Governing: SPEC-0020 REQ "Expired Link Resolution", REQ "Archived Link Resolution", ADR-0020
package handler

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/metrics"
	"github.com/joestump/joe-links/internal/store"
)

// varPlaceholderRe matches $varname placeholders in URL templates.
// Governing: SPEC-0009 REQ "Variable Substitution and Redirect", ADR-0013
var varPlaceholderRe = regexp.MustCompile(`\$[a-z][a-z0-9_]*`)

// ResolveHandler handles short link slug resolution and redirection.
// Governing: SPEC-0016 REQ "Click Recording", ADR-0016
type ResolveHandler struct {
	links     *store.LinkStore
	keywords  *store.KeywordStore
	ownership *store.OwnershipStore
	clickCh   chan<- store.ClickEvent
}

// NewResolveHandler creates a new ResolveHandler.
// If clickCh is nil, click recording is disabled.
func NewResolveHandler(ls *store.LinkStore, ks *store.KeywordStore, os *store.OwnershipStore, clickCh chan<- store.ClickEvent) *ResolveHandler {
	return &ResolveHandler{links: ls, keywords: ks, ownership: os, clickCh: clickCh}
}

type notFoundPage struct {
	BasePage
	User *store.User
	Slug string
	// Candidate is the normalized slug the create CTA pre-fills — the first
	// path segment for multi-segment misses. Creatable gates the CTA
	// server-side: reserved or format-invalid paths get no create offer at
	// all, for signed-in and anonymous visitors alike (issue #260).
	Candidate string
	Creatable bool
	Flash     *Flash
	// Suggestions are the did-you-mean slugs rendered above the create CTA,
	// nearest first. Empty (the common case) renders the page exactly as it
	// did before SPEC-0019.
	// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions"
	Suggestions []string
}

// creatableCandidate returns the slug the 404 page may offer to create for a
// missed path, and whether that offer is valid. Slugs cannot contain "/", so
// for multi-segment paths the first segment is the candidate — creating it as
// a variable link is the only way the missed path could ever resolve. Format
// and reserved-slug checks are delegated to store.ValidateSlugFormat, the
// single source of truth for slug rules (issue #260).
// Governing: SPEC-0002 REQ "Slug Uniqueness and Format Validation", ADR-0005
// Governing: SPEC-0004 REQ "Slug Resolver and 404 Page"
func creatableCandidate(path string) (string, bool) {
	candidate, _, _ := strings.Cut(path, "/")
	if err := store.ValidateSlugFormat(candidate); err != nil {
		return "", false
	}
	return candidate, true
}

// Resolve looks up a slug and redirects to the target URL, or renders a 404 page.
// Governing: SPEC-0001 REQ "HTMX Hypermedia Interactions"
// Governing: SPEC-0009 REQ "Multi-Segment Path Resolution", ADR-0013
func (h *ResolveHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	// Governing: SPEC-0016 REQ "Prometheus Metrics Endpoint", ADR-0016
	start := time.Now()
	defer func() { metrics.RedirectDuration.Observe(time.Since(start).Seconds()) }()

	// Extract the full path after the leading "/".
	// Governing: SPEC-0009 REQ "Multi-Segment Path Resolution", ADR-0013
	fullPath := strings.TrimPrefix(r.URL.Path, "/")
	if fullPath == "" {
		metrics.RedirectsTotal.WithLabelValues("not_found").Inc()
		h.render404(w, r, "", true)
		return
	}

	host := strings.SplitN(r.Host, ":", 2)[0]

	// Path-based keyword routing: /{keyword}/{slug} on the main server.
	// The browser extension redirects to {baseURL}/{keyword}/{slug} when the
	// keyword hostname isn't the server itself (Firefox fallback).
	// Governing: SPEC-0008 REQ "Search Interception and Redirect"
	parts := strings.SplitN(fullPath, "/", 2)
	if len(parts) == 2 && parts[1] != "" && parts[0] != host {
		if kw, err := h.keywords.GetByKeyword(r.Context(), parts[0]); err == nil {
			target := strings.ReplaceAll(kw.URLTemplate, "{slug}", parts[1])
			metrics.RedirectsTotal.WithLabelValues("found").Inc()
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
	}

	// Governing: ADR-0011 — check if request host is a registered keyword.
	kw, kwErr := h.keywords.GetByKeyword(r.Context(), host)
	if kwErr == nil {
		// Substitute {slug} in the URL template and redirect.
		target := strings.ReplaceAll(kw.URLTemplate, "{slug}", fullPath)
		metrics.RedirectsTotal.WithLabelValues("found").Inc()
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	// kwErr == store.ErrNotFound → fall through to normal slug resolution

	// Step 1: Try exact slug match on the full path. The candidate is
	// case-folded before lookup: stored slugs are canonically lowercase
	// (SPEC-0002), so `/JIRA` must find `jira` without any DB collation or
	// schema change — creation validation still rejects uppercase, keeping the
	// stored corpus canonical (ADR-0019).
	// Governing: SPEC-0009 REQ "Multi-Segment Path Resolution" — exact match wins
	// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution"
	lookupPath := strings.ToLower(fullPath)
	link, err := h.links.GetBySlug(r.Context(), lookupPath)
	if err == store.ErrNotFound {
		// Normalization forgiveness: retry the failed case-folded exact lookup
		// with underscores swapped for hyphens and trailing sentence
		// punctuation stripped, before prefix matching and the 404 path.
		// Resolution lookups only — creation, update, and uniqueness checks
		// stay strict — and a normalized match flows through the same
		// visibility enforcement below as an exact match.
		// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
		for _, candidate := range normalizationCandidates(lookupPath) {
			if l, cErr := h.links.GetBySlug(r.Context(), candidate); cErr == nil {
				link, err = l, nil
				break
			}
		}
	}
	if err == nil {
		// Governing: SPEC-0010 REQ "Secure Link Resolution", REQ "Public Link Resolution", REQ "Private Link Resolution"
		if !h.checkVisibility(w, r, link) {
			return
		}
		// Lifecycle runs strictly after the visibility gate so no expired or
		// archived rendering can become an existence oracle for secure slugs.
		// Governing: SPEC-0020 Security "Resolution Ordering and Oracle Resistance", ADR-0020
		if !h.checkLifecycle(w, r, link) {
			return
		}
		// A variable link visited with no variable segments would redirect to
		// the literal placeholder URL (e.g. https://.../browse/$ticket). Treat
		// it as an arity mismatch (zero provided), consistent with Step 2.
		// Governing: SPEC-0009 REQ "Variable Substitution and Redirect", ADR-0013
		if varPlaceholderRe.MatchString(link.URL) {
			metrics.RedirectsTotal.WithLabelValues("not_found").Inc()
			// The slug just matched an existing link — a create CTA here is a
			// guaranteed ErrSlugTaken dead end (issue #260).
			h.render404(w, r, fullPath, false)
			return
		}
		metrics.RedirectsTotal.WithLabelValues("found").Inc()
		h.redirect(w, r, link.ID, link.URL)
		return
	}

	// Step 2: Try progressively shorter prefixes for multi-segment paths.
	// Governing: SPEC-0009 REQ "Multi-Segment Path Resolution", ADR-0013
	segments := strings.Split(fullPath, "/")
	if len(segments) > 1 {
		for i := len(segments) - 1; i >= 1; i-- {
			// Case-folding applies uniformly to every prefix candidate; the
			// remaining segments are taken from the original path below, so
			// variable values reach substitution with their case preserved.
			// Governing: SPEC-0019 REQ "Case-Insensitive Slug Resolution"
			prefix := strings.ToLower(strings.Join(segments[:i], "/"))
			link, err := h.links.GetBySlug(r.Context(), prefix)
			if err != nil {
				continue
			}

			// Governing: SPEC-0009 REQ "Variable Substitution and Redirect" — original case preserved
			remaining := segments[i:]

			// Governing: SPEC-0010 REQ "Secure Link Resolution"
			if !h.checkVisibility(w, r, link) {
				return
			}

			// An expired or archived prefix match terminates resolution — the
			// resolver commits to the first visibility-passing match and never
			// falls through to shorter prefixes, preserving today's
			// stop-at-first-visible-match semantics.
			// Governing: SPEC-0020 REQ "Expired Link Resolution" scenario "Expired Prefix Match Terminates Resolution"
			if !h.checkLifecycle(w, r, link) {
				return
			}

			// Check if URL contains $varname placeholders.
			// Governing: SPEC-0009 REQ "Variable Substitution and Redirect", ADR-0013
			placeholders := varPlaceholderRe.FindAllString(link.URL, -1)
			if len(placeholders) == 0 {
				// Static link — redirect as-is.
				metrics.RedirectsTotal.WithLabelValues("found").Inc()
				h.redirect(w, r, link.ID, link.URL)
				return
			}

			// Deduplicate placeholders preserving order of first appearance.
			seen := make(map[string]bool)
			var unique []string
			for _, p := range placeholders {
				if !seen[p] {
					seen[p] = true
					unique = append(unique, p)
				}
			}

			// Arity check: remaining segments must equal unique placeholder count.
			if len(remaining) != len(unique) {
				metrics.RedirectsTotal.WithLabelValues("not_found").Inc()
				// Resolution already matched an existing prefix link, and
				// longer prefixes win before shorter ones — creating the first
				// segment could never make this path resolve (issue #260).
				h.render404(w, r, fullPath, false)
				return
			}

			// Substitute positionally with url.PathEscape in a single regex
			// pass. Sequential ReplaceAll corrupted templates whose variable
			// names share a prefix ($env rewrote the $env inside $env_id) and
			// re-scanned already-substituted values for placeholders.
			values := make(map[string]string, len(unique))
			for j, placeholder := range unique {
				values[placeholder] = url.PathEscape(remaining[j])
			}
			target := varPlaceholderRe.ReplaceAllStringFunc(link.URL, func(m string) string {
				return values[m]
			})

			metrics.RedirectsTotal.WithLabelValues("found").Inc()
			h.redirect(w, r, link.ID, target)
			return
		}
	}

	// No match found → 404.
	metrics.RedirectsTotal.WithLabelValues("not_found").Inc()
	h.render404(w, r, fullPath, true)
}

// trailingPunct is the set of sentence punctuation forgiven at the end of a
// requested path — what clings to a go/slug link pasted at the end of a
// sentence or inside parentheses.
// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
const trailingPunct = ".,;:!?)"

// normalizationCandidates returns the forgiving retry lookups for a
// case-folded path whose exact lookup failed: underscores replaced with
// hyphens, trailing punctuation stripped, and both together. Candidates equal
// to the already-tried path, empty results, and duplicates are omitted;
// order is deterministic. This is a resolution-only forgiveness — slug
// creation, update, and uniqueness checks never normalize
// (store.ValidateSlugFormat stays the single source of truth for slug rules).
// Governing: SPEC-0019 REQ "Slug Normalization Forgiveness"
func normalizationCandidates(path string) []string {
	// Slugs cannot contain "/" (SPEC-0002), so every candidate derived from a
	// multi-segment path is a guaranteed miss — skip straight to prefix
	// matching instead of burning up to three pointless lookups. Forgiveness
	// applies only to the whole-path exact lookup, per the spec's "before
	// falling through to prefix matching".
	if strings.Contains(path, "/") {
		return nil
	}
	hyphenated := strings.ReplaceAll(path, "_", "-")
	seen := map[string]bool{path: true}
	var out []string
	for _, candidate := range []string{
		hyphenated,
		strings.TrimRight(path, trailingPunct),
		strings.TrimRight(hyphenated, trailingPunct),
	} {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

// checkVisibility enforces visibility rules for a link.
// Returns true if the request is allowed to proceed to redirect.
// Returns false if it has already written a response (login redirect or 403).
// Governing: SPEC-0010 REQ "Secure Link Resolution", REQ "Public Link Resolution", REQ "Private Link Resolution", REQ "Admin Visibility Override"
func (h *ResolveHandler) checkVisibility(w http.ResponseWriter, r *http.Request, link *store.Link) bool {
	switch link.Visibility {
	case "public", "private":
		// Governing: SPEC-0010 REQ "Public Link Resolution" — 302 for anyone
		// Governing: SPEC-0010 REQ "Private Link Resolution" — 302 for anyone who knows the slug
		return true
	case "secure":
		user := auth.UserFromContext(r.Context())
		if user == nil {
			// Governing: SPEC-0010 REQ "Secure Link Resolution" — redirect to login with return_url
			returnURL := r.URL.RequestURI()
			http.Redirect(w, r, "/auth/login?return_url="+url.QueryEscape(returnURL), http.StatusFound)
			return false
		}
		// Governing: SPEC-0010 REQ "Admin Visibility Override" — admins always authorized
		if user.IsAdmin() {
			return true
		}
		// Check if user is an owner/co-owner
		isOwner, err := h.ownership.IsOwner(link.ID, user.ID)
		if err == nil && isOwner {
			return true
		}
		// Check link_shares
		hasShare, err := h.links.HasShare(r.Context(), link.ID, user.ID)
		if err == nil && hasShare {
			return true
		}
		// Not authorized — shared styled 403 renderer
		RenderForbidden(w, r)
		return false
	default:
		// Unknown visibility — treat as public
		return true
	}
}

// checkLifecycle enforces the link's derived lifecycle state. It MUST be
// called only after checkVisibility has passed: the visibility gate runs
// first for every path (exact match, prefix resolution, HEAD), so a secure
// link's login redirect / 403 is byte-identical whether or not the link is
// expired or archived and no lifecycle response can become an existence
// oracle. Returns true when the link is active and resolution may proceed;
// returns false after writing the terminal lifecycle response. Neither
// terminal outcome redirects or records a click event.
// Governing: SPEC-0020 REQ "Expired Link Resolution", REQ "Archived Link Resolution", ADR-0020
// Governing: SPEC-0020 Security "Resolution Ordering and Oracle Resistance"
func (h *ResolveHandler) checkLifecycle(w http.ResponseWriter, r *http.Request, link *store.Link) bool {
	// Archived wins when both archived_at and a past expires_at apply.
	// Governing: SPEC-0020 REQ "Archived Link Resolution" scenario "Archived Beats Expired in Derived State"
	switch link.LifecycleState(time.Now().UTC()) {
	case store.LifecycleArchived:
		// Archived presents as gone: the standard 404 page (never 410), minus
		// the Create CTA — the slug stays reserved by the archived row. The
		// page names the matched slug so a prefix match reads like an exact
		// match instead of naming the full requested path.
		// Governing: SPEC-0020 REQ "Archived Link Resolution"
		metrics.RedirectsTotal.WithLabelValues("not_found").Inc()
		h.render404(w, r, link.Slug, false)
		return false
	case store.LifecycleExpired:
		// Governing: SPEC-0020 REQ "Expired Link Resolution"
		metrics.RedirectsTotal.WithLabelValues("not_found").Inc()
		h.renderExpired(w, r, link)
		return false
	}
	return true
}

// expiredPage is the template data for the "this link has expired" page.
// Governing: SPEC-0020 REQ "Expired Link Resolution"
type expiredPage struct {
	BasePage
	User *store.User
	Slug string
	// OwnerName/OwnerSlug identify the link's primary owner. Populated only
	// when the link is public or the viewer holds CanView — other viewers of
	// a private expired link get no owner identity.
	OwnerName string
	OwnerSlug string
}

// renderExpired renders the styled expired page for a link whose derived
// state is expired. The status is 404 Not Found — never 410 Gone — so the
// status-code surface cannot distinguish "existed once" from "never existed"
// (ADR-0020). The owner is named (linking to their public profile, SPEC-0012)
// only when the link's visibility is public or the viewer holds CanView on
// the link. The page never offers the 404 page's Create CTA: the slug remains
// reserved by the expired link.
// Governing: SPEC-0020 REQ "Expired Link Resolution", ADR-0020
func (h *ResolveHandler) renderExpired(w http.ResponseWriter, r *http.Request, link *store.Link) {
	user := auth.UserFromContext(r.Context())

	showOwner := link.Visibility == "public"
	if !showOwner && user != nil {
		if caps, err := store.LinkCapsFor(r.Context(), h.ownership, h.links, link.ID, user); err == nil && caps.CanView {
			showOwner = true
		}
	}
	ownerName, ownerSlug := "", ""
	if showOwner {
		// ListOwnerUsers orders the primary owner first (SPEC-0002).
		if owners, err := h.ownership.ListOwnerUsers(link.ID); err == nil && len(owners) > 0 {
			ownerName = owners[0].DisplayName
			ownerSlug = owners[0].DisplayNameSlug
		}
	}

	w.WriteHeader(http.StatusNotFound)
	data := expiredPage{BasePage: newBasePage(r, user), User: user, Slug: link.Slug, OwnerName: ownerName, OwnerSlug: ownerSlug}
	// Governing: SPEC-0020 REQ "Expired Link Resolution" — HX-Request renders the same content as a fragment (SPEC-0004 conventions)
	if isHTMX(r) {
		renderPageFragment(w, "expired.html", "content", data)
		return
	}
	render(w, "expired.html", data)
}

// redirect issues a 302 redirect, handling HTMX requests with HX-Redirect header.
// It also fires a non-blocking click event if the click channel is configured.
// Governing: SPEC-0016 REQ "Click Recording", ADR-0016
func (h *ResolveHandler) redirect(w http.ResponseWriter, r *http.Request, linkID, target string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
	} else {
		http.Redirect(w, r, target, http.StatusFound)
	}

	// HEAD probes (link checkers, unfurl bots, curl -I) are not visits;
	// recording them would inflate click stats.
	// Governing: SPEC-0016 REQ "Click Recording", ADR-0016
	if h.clickCh != nil && r.Method != http.MethodHead {
		var userID string
		if u := auth.UserFromContext(r.Context()); u != nil {
			userID = u.ID
		}
		// User-agent and referrer length limits are enforced rune-safely in
		// ClickStore.RecordClick (512 / 2048 runes). Byte-slicing here first
		// could split a multi-byte character at the cut point and hand invalid
		// UTF-8 downstream, so the store's truncation is the only cap (issue #205).
		ua := r.UserAgent()
		// Governing: SPEC-0016 REQ "Click Data Schema" — strip query/fragment to prevent token leakage
		ref := r.Referer()
		if ref != "" {
			if u, err := url.Parse(ref); err == nil {
				u.RawQuery = ""
				u.Fragment = ""
				ref = u.String()
			}
		}
		select {
		case h.clickCh <- store.ClickEvent{
			LinkID:    linkID,
			UserID:    userID,
			IPHash:    store.HashIP(realIP(r)),
			UserAgent: ua,
			Referrer:  ref,
		}:
		default: // Governing: SPEC-0016 REQ "Click Recording"
			log.Printf("analytics: click channel full, dropping event for link %s", linkID)
		}
	}
}

// realIP extracts the client IP from r.RemoteAddr (port stripped).
// Chi's middleware.RealIP already rewrites r.RemoteAddr from X-Real-IP / X-Forwarded-For.
func realIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Did-you-mean bounds, enforced server-side (SPEC-0019): only candidates
// within plain Levenshtein distance 2 qualify, and at most 3 render.
// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions", ADR-0019
const (
	maxDidYouMeanDistance    = 2
	maxDidYouMeanSuggestions = 3
)

// levenshtein returns the plain Levenshtein edit distance between a and b in
// runes: insertions, deletions, and substitutions each cost 1. ADR-0019 chose
// plain distance — no transposition move — so a swap like "jria"→"jira" costs
// 2 and still qualifies under the ≤2 bound. Two-row DP, O(len(a)·len(b)).
// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions", ADR-0019
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

// didYouMeanSuggestions computes the 404 page's "Did you mean" slugs for a
// missed path. Only the first path segment is matched, lowercased to align
// with the canonically lowercase slug corpus (SPEC-0002); empty and
// single-character paths get no suggestions. Candidates come from the store
// visibility-filtered to what the viewer may discover (anonymous → public
// only; authenticated → plus own/co-owned/shared; admin → all; expired and
// archived always excluded) and length-bounded in SQL; the distance bound
// (≤2), ordering (ascending distance, ties by slug ascending in byte order),
// and result cap (3) are enforced here, in Go, per ADR-0019. Suggestions are
// best-effort: on store error the 404 renders without them. This runs only
// after case-folded exact lookup, normalization forgiveness, and prefix
// matching have all missed — render404 is the end of the resolution chain.
// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions", ADR-0019
func (h *ResolveHandler) didYouMeanSuggestions(r *http.Request, path string) []string {
	first, _, _ := strings.Cut(path, "/")
	q := strings.ToLower(first)
	if len([]rune(q)) <= 1 {
		return nil
	}
	viewerID, isAdmin := "", false
	if user := auth.UserFromContext(r.Context()); user != nil {
		viewerID, isAdmin = user.ID, user.IsAdmin()
	}
	candidates, err := h.links.DidYouMeanCandidates(r.Context(), viewerID, isAdmin, q)
	if err != nil {
		log.Printf("did-you-mean: candidate query failed for %q: %v", q, err)
		return nil
	}
	type scored struct {
		slug string
		dist int
	}
	var qualified []scored
	for _, slug := range candidates {
		// A candidate equal to the requested segment is the very slug that
		// just failed to resolve (an archived match or a variable link with
		// the wrong arity) — suggesting it would link straight back to this
		// 404.
		if slug == q {
			continue
		}
		if d := levenshtein(q, slug); d <= maxDidYouMeanDistance {
			qualified = append(qualified, scored{slug: slug, dist: d})
		}
	}
	sort.Slice(qualified, func(i, j int) bool {
		if qualified[i].dist != qualified[j].dist {
			return qualified[i].dist < qualified[j].dist
		}
		// Go string comparison is byte order on every platform, unlike SQL
		// collations (see SuggestLinks).
		return qualified[i].slug < qualified[j].slug
	})
	if len(qualified) > maxDidYouMeanSuggestions {
		qualified = qualified[:maxDidYouMeanSuggestions]
	}
	out := make([]string, len(qualified))
	for i, s := range qualified {
		out[i] = s.slug
	}
	return out
}

// render404 renders the 404 page for a missing slug. offerCreate=false
// suppresses the create CTA unconditionally — used when resolution already
// matched an existing link (a variable link visited with the wrong arity), so
// creating the candidate slug is guaranteed to fail with ErrSlugTaken.
func (h *ResolveHandler) render404(w http.ResponseWriter, r *http.Request, slug string, offerCreate bool) {
	user := auth.UserFromContext(r.Context())
	// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions" — computed
	// before the header is written, for the full page and the HTMX fragment
	// alike.
	suggestions := h.didYouMeanSuggestions(r, slug)
	w.WriteHeader(http.StatusNotFound)
	// Only offer creation for paths that could actually become links —
	// a CTA that lands on an immediate validation error is a dead end
	// (issue #260).
	candidate, creatable := "", false
	if offerCreate {
		candidate, creatable = creatableCandidate(slug)
	}
	data := notFoundPage{BasePage: newBasePage(r, user), User: user, Slug: slug, Candidate: candidate, Creatable: creatable, Suggestions: suggestions}
	if isHTMX(r) {
		renderPageFragment(w, "404.html", "content", data)
		return
	}
	render(w, "404.html", data)
}
