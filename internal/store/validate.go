// Governing: SPEC-0002 REQ "Slug Uniqueness and Format Validation", ADR-0005
package store

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Link text length limits.
// Governing: SPEC-0002 REQ "Links Table" — title max 200, description max 2000 characters
const (
	MaxTitleLength       = 200
	MaxDescriptionLength = 2000
)

// Tag intake limits (issues #251, #265): tag display names are rendered on
// every tag surface, so they get a safe charset at intake, and each write may
// attach only a bounded number of tags so a single request cannot fan out into
// unbounded per-tag DB work.
const (
	MaxTagNameLength = 50
	MaxTagsPerLink   = 20
)

var (
	// ErrSlugInvalid is returned when a slug does not match the required pattern.
	ErrSlugInvalid = errors.New("slug must match [a-z0-9][a-z0-9-]*[a-z0-9]")

	// ErrTitleTooLong is returned when a title exceeds MaxTitleLength characters.
	// Governing: SPEC-0002 REQ "Links Table" scenario "Title Exceeds Maximum Length"
	ErrTitleTooLong = fmt.Errorf("title must be at most %d characters", MaxTitleLength)

	// ErrDescriptionTooLong is returned when a description exceeds MaxDescriptionLength characters.
	// Governing: SPEC-0002 REQ "Links Table" scenario "Description Exceeds Maximum Length"
	ErrDescriptionTooLong = fmt.Errorf("description must be at most %d characters", MaxDescriptionLength)

	// ErrSlugReserved is returned when a slug matches a reserved route prefix.
	ErrSlugReserved = errors.New("slug is reserved and cannot be used")

	// ErrSlugTaken is returned when a slug already exists in the database.
	ErrSlugTaken = errors.New("slug is already taken")

	// ErrDuplicateVariable is returned when a URL template contains duplicate $varname placeholders.
	// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
	ErrDuplicateVariable = errors.New("duplicate variable name in URL template")

	// ErrInvalidVisibility is returned when a visibility value is not one of public, private, secure.
	// Governing: SPEC-0010 REQ "Visibility Column on Links Table"
	ErrInvalidVisibility = errors.New("visibility must be one of: public, private, secure")

	// ErrURLSchemeInvalid is returned when a link URL does not use the http or
	// https scheme. javascript:/data:/etc URLs are stored-XSS primitives the
	// moment any redirect surface hands them to the browser, so they are
	// rejected at intake on every write path (issue #265).
	ErrURLSchemeInvalid = errors.New("url must start with http:// or https://")

	// ErrURLHostMissing is returned when a link URL carries an http(s) scheme
	// but no host component ("http://", "https:", "http:opaque"). Not a
	// security concern — the scheme allowlist already won — but such a
	// destination can never resolve, so it is rejected as a typo (PR #280
	// review follow-up).
	ErrURLHostMissing = errors.New("url must include a host (e.g. https://example.com)")

	// ErrTagNameInvalid is returned when a tag display name falls outside the
	// safe charset. Defense in depth for the stored XSS class fixed at the
	// output layer in issue #246 (issue #251).
	ErrTagNameInvalid = errors.New("tag names must start with a letter or digit and may contain only letters, digits, spaces, hyphens, and underscores")

	// ErrTagNameTooLong is returned when a tag display name exceeds MaxTagNameLength characters.
	ErrTagNameTooLong = fmt.Errorf("tag names must be at most %d characters", MaxTagNameLength)

	// ErrTooManyTags is returned when a single write attaches more than MaxTagsPerLink tags.
	ErrTooManyTags = fmt.Errorf("a link may have at most %d tags", MaxTagsPerLink)

	slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`)

	// tagNameRe is the safe charset for tag display names: must start
	// alphanumeric, then letters, digits, spaces, hyphens, underscores. ASCII
	// alphanumerics only, consistent with DeriveTagSlug — a name accepted here
	// always derives a non-empty slug, so hostile or unroutable
	// empty-slug tags cannot be created (issue #251).
	tagNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 _-]*$`)

	// VarPlaceholderRe matches $varname placeholders in URL templates.
	// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
	VarPlaceholderRe = regexp.MustCompile(`\$[a-z][a-z0-9_]*`)

	// reservedSlugs is the single source of truth for slugs that collide with
	// application routes. Reservation is exact-match only: routes are
	// path-segmented (/u/{...} and /u-foo are distinct), so dash-prefixed
	// slugs like "u-foo" or "links-roundup" are valid (issue #204).
	// Every entry must correspond to a top-level route in internal/handler/router.go.
	// Governing: SPEC-0001 REQ "Short Link Resolution" — reserved routes MUST NOT be valid slugs
	reservedSlugs = map[string]bool{
		"auth":      true, // Governing: SPEC-0004 REQ "Route Registration and Priority"
		"static":    true, // Governing: SPEC-0004 REQ "Route Registration and Priority"
		"dashboard": true, // Governing: SPEC-0004 REQ "Route Registration and Priority"
		"admin":     true, // Governing: SPEC-0004 REQ "Route Registration and Priority"
		"api":       true, // Governing: SPEC-0005 REQ "API Router Mounting" — shadows /api/v1/* and /api/docs/* routes
		"u":         true, // Governing: SPEC-0012 REQ "User Profile Route Priority"
		"links":     true, // Governing: SPEC-0012 REQ "Public Link Browser Route Priority"
		"metrics":   true, // Governing: SPEC-0016 REQ "Prometheus Metrics Endpoint", ADR-0016
		"mcp":       true, // Governing: SPEC-0018 REQ "MCP Endpoint", ADR-0018
	}

	// reservedSlugList is the sorted form of reservedSlugs, used to derive
	// user-facing messages so they can never drift from the actual set.
	reservedSlugList = func() []string {
		out := make([]string, 0, len(reservedSlugs))
		for s := range reservedSlugs {
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	}()
)

// ReservedSlugs returns the sorted list of slugs reserved for application
// routes. Callers must not mutate the returned slice's source; a fresh copy
// is returned on each call.
func ReservedSlugs() []string {
	out := make([]string, len(reservedSlugList))
	copy(out, reservedSlugList)
	return out
}

// ValidateSlugFormat checks that slug conforms to the required format and is
// not reserved. It does NOT check uniqueness — that is handled at the database
// layer via the unique index on links.slug.
func ValidateSlugFormat(slug string) error {
	if !slugRe.MatchString(slug) {
		return ErrSlugInvalid
	}
	if reservedSlugs[slug] {
		// Name the full reserved set in the message, derived from the map so
		// user-facing errors can never drift from the actual rule (issue #204).
		return fmt.Errorf("%w: %q (reserved: %s)", ErrSlugReserved, slug, strings.Join(reservedSlugList, ", "))
	}
	return nil
}

// ValidateLinkText checks that title and description do not exceed their
// maximum character lengths. Length is measured in Unicode code points (runes),
// matching the spec's "characters" wording.
// Governing: SPEC-0002 REQ "Links Table" scenarios "Title/Description Exceeds Maximum Length"
func ValidateLinkText(title, description string) error {
	if utf8.RuneCountInString(title) > MaxTitleLength {
		return ErrTitleTooLong
	}
	if utf8.RuneCountInString(description) > MaxDescriptionLength {
		return ErrDescriptionTooLong
	}
	return nil
}

// ValidateURLVariables checks that any $varname placeholders in url are unique.
// Returns nil if the URL contains no variables or all variable names are distinct.
// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
func ValidateURLVariables(url string) error {
	vars := VarPlaceholderRe.FindAllString(url, -1)
	if len(vars) <= 1 {
		return nil
	}
	seen := make(map[string]bool, len(vars))
	for _, v := range vars {
		if seen[v] {
			return fmt.Errorf("%w: %s", ErrDuplicateVariable, v)
		}
		seen[v] = true
	}
	return nil
}

// ValidateVisibility checks that v is one of the allowed visibility values.
// Governing: SPEC-0010 REQ "Visibility Column on Links Table"
func ValidateVisibility(v string) error {
	switch v {
	case "public", "private", "secure":
		return nil
	default:
		return ErrInvalidVisibility
	}
}

// ValidateLinkURL checks that a link destination URL parses, uses the http or
// https scheme, and names a host. Everything else — javascript:, data:,
// vbscript:, scheme-relative //host, or bare paths — is rejected: the
// resolver, the browser extension, and SPEC-0020's health checker all assume
// http(s) destinations, and non-http schemes are latent stored-XSS payloads on
// any HTMX-driven redirect surface (issue #265). Enforced on every write
// surface; existing rows are not migrated.
// Governing: SPEC-0002 REQ "Links Table" — url column
func ValidateLinkURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ErrURLSchemeInvalid
	}
	// url.Parse lowercases the scheme, so this also rejects JaVaScRiPt: forms.
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrURLSchemeInvalid
	}
	// The scheme alone is not enough: "http://", "https:", and "http:opaque"
	// all parse with a valid scheme but an empty host and can never resolve.
	if u.Host == "" {
		return ErrURLHostMissing
	}
	return nil
}

// ValidateTagName checks a single tag display name (after trimming, matching
// how the tag store persists names): safe charset, must start alphanumeric,
// bounded length. Names that would derive an empty slug (e.g. all-symbol or
// non-ASCII names) are rejected here for free, so no unroutable slug='' tag
// row can be created (issue #251).
// Governing: SPEC-0002 REQ "Tags Table" — every stored name derives a non-empty slug
func ValidateTagName(name string) error {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxTagNameLength {
		return fmt.Errorf("%w: %q", ErrTagNameTooLong, name)
	}
	if !tagNameRe.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrTagNameInvalid, name)
	}
	return nil
}

// ValidateTagNames checks a full tag list as submitted by one write: at most
// MaxTagsPerLink entries, each passing ValidateTagName (issues #251, #265).
// Governing: SPEC-0002 REQ "Tags Table"
func ValidateTagNames(names []string) error {
	if len(names) > MaxTagsPerLink {
		return ErrTooManyTags
	}
	for _, name := range names {
		if err := ValidateTagName(name); err != nil {
			return err
		}
	}
	return nil
}

// likeEscaper escapes the LIKE metacharacters % and _ (and the escape
// character itself) so user-supplied search terms match literally instead of
// acting as wildcards. Every query using an escaped pattern MUST carry
// ESCAPE '!'. '!' is used instead of the conventional backslash because
// backslash is itself an escape character inside MySQL string literals, so a
// single `... ESCAPE '\'` query text cannot be portable across
// sqlite3/mysql/postgres (issue #265).
var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// escapeLike returns s with LIKE metacharacters escaped for use inside a
// pattern matched with ESCAPE '!'.
func escapeLike(s string) string { return likeEscaper.Replace(s) }
