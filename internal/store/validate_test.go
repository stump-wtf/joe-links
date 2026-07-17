// Governing: SPEC-0002 REQ "Slug Uniqueness and Format Validation"
// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
package store

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSlugFormat(t *testing.T) {
	tests := []struct {
		name    string
		slug    string
		wantErr error
	}{
		// Valid slugs
		{name: "single lowercase letter", slug: "a", wantErr: nil},
		{name: "single digit", slug: "5", wantErr: nil},
		{name: "two characters", slug: "ab", wantErr: nil},
		{name: "simple word", slug: "docs", wantErr: nil},
		{name: "with hyphens", slug: "my-link", wantErr: nil},
		{name: "multiple hyphens", slug: "my-cool-link", wantErr: nil},
		{name: "digits and letters", slug: "go2docs", wantErr: nil},
		{name: "digits with hyphens", slug: "1-2-3", wantErr: nil},
		{name: "consecutive hyphens", slug: "my--link", wantErr: nil},

		// Format violations
		{name: "empty string", slug: "", wantErr: ErrSlugInvalid},
		{name: "uppercase letters", slug: "MyLink", wantErr: ErrSlugInvalid},
		{name: "mixed case", slug: "myLink", wantErr: ErrSlugInvalid},
		{name: "starts with hyphen", slug: "-foo", wantErr: ErrSlugInvalid},
		{name: "ends with hyphen", slug: "foo-", wantErr: ErrSlugInvalid},
		{name: "only a hyphen", slug: "-", wantErr: ErrSlugInvalid},
		{name: "contains spaces", slug: "my link", wantErr: ErrSlugInvalid},
		{name: "contains underscore", slug: "my_link", wantErr: ErrSlugInvalid},
		{name: "contains period", slug: "my.link", wantErr: ErrSlugInvalid},
		{name: "contains slash", slug: "my/link", wantErr: ErrSlugInvalid},

		// Reserved slugs — every routed prefix, exact match (issue #204)
		{name: "reserved auth", slug: "auth", wantErr: ErrSlugReserved},
		{name: "reserved static", slug: "static", wantErr: ErrSlugReserved},
		{name: "reserved dashboard", slug: "dashboard", wantErr: ErrSlugReserved},
		{name: "reserved admin", slug: "admin", wantErr: ErrSlugReserved},
		{name: "reserved api", slug: "api", wantErr: ErrSlugReserved},         // Governing: SPEC-0005 REQ "API Router Mounting"
		{name: "reserved u", slug: "u", wantErr: ErrSlugReserved},             // Governing: SPEC-0012 REQ "User Profile Route Priority"
		{name: "reserved links", slug: "links", wantErr: ErrSlugReserved},     // Governing: SPEC-0012 REQ "Public Link Browser Route Priority"
		{name: "reserved metrics", slug: "metrics", wantErr: ErrSlugReserved}, // Governing: SPEC-0016 REQ "Prometheus Metrics Endpoint"
		{name: "reserved mcp", slug: "mcp", wantErr: ErrSlugReserved},         // Governing: SPEC-0018 REQ "MCP Endpoint"

		// Not reserved: reservation is exact-match only — routes are
		// path-segmented, so dash-prefixed slugs create no conflict (#204).
		{name: "u-foo not reserved", slug: "u-foo", wantErr: nil},
		{name: "links-roundup not reserved", slug: "links-roundup", wantErr: nil},
		{name: "api-test not reserved", slug: "api-test", wantErr: nil},
		{name: "mcp-notes not reserved", slug: "mcp-notes", wantErr: nil},
		{name: "auth-settings not reserved", slug: "auth-settings", wantErr: nil},
		{name: "myadmin not reserved", slug: "myadmin", wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSlugFormat(tt.slug)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("ValidateSlugFormat(%q) = %v, want nil", tt.slug, err)
				}
				return
			}
			if err == nil {
				t.Errorf("ValidateSlugFormat(%q) = nil, want %v", tt.slug, tt.wantErr)
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidateSlugFormat(%q) = %v, want error wrapping %v", tt.slug, err, tt.wantErr)
			}
		})
	}
}

// TestReservedSlugs_AllRejected asserts every exported reserved slug is
// rejected by the single validation entry point. The set's correspondence to
// the actual route table is pinned by
// handler.TestReservedSlugs_CoverEveryTopLevelRoute, which derives the routes
// from the real router rather than a second hand-maintained list (#204).
func TestReservedSlugs_AllRejected(t *testing.T) {
	got := ReservedSlugs()
	if len(got) == 0 {
		t.Fatal("ReservedSlugs() is empty")
	}
	for _, slug := range got {
		if err := ValidateSlugFormat(slug); !errors.Is(err, ErrSlugReserved) {
			t.Errorf("ValidateSlugFormat(%q) = %v, want ErrSlugReserved", slug, err)
		}
	}
}

// TestValidateSlugFormat_ReservedMessageNamesFullSet guards the user-facing
// message: it is derived from the reserved set, so it must name every
// reserved word and cannot drift from the rule (#204).
func TestValidateSlugFormat_ReservedMessageNamesFullSet(t *testing.T) {
	err := ValidateSlugFormat("mcp")
	if err == nil {
		t.Fatal("ValidateSlugFormat(\"mcp\") = nil, want ErrSlugReserved")
	}
	for _, slug := range ReservedSlugs() {
		if !strings.Contains(err.Error(), slug) {
			t.Errorf("reserved error %q does not name reserved slug %q", err.Error(), slug)
		}
	}
}

// Governing: SPEC-0009 REQ "Variable Placeholder Syntax", ADR-0013
func TestValidateURLVariables(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr error
	}{
		// Static URLs (no variables) — always valid
		{name: "static URL", url: "https://example.com", wantErr: nil},
		{name: "empty URL", url: "", wantErr: nil},

		// Valid single variable
		{name: "single variable", url: "https://github.com/$username", wantErr: nil},
		{name: "variable in query", url: "https://example.com/search?q=$query", wantErr: nil},
		{name: "variable with digits", url: "https://example.com/$var1", wantErr: nil},
		{name: "variable with underscores", url: "https://example.com/$my_var", wantErr: nil},

		// Valid multiple distinct variables
		{name: "two distinct variables", url: "https://example.com/$foo/$bar", wantErr: nil},
		{name: "multiple in query params", url: "https://example.com/?q=$query&page=$page", wantErr: nil},
		{name: "three variables", url: "https://example.com/$a/$b/$c", wantErr: nil},

		// Duplicate variable names — rejected
		{name: "duplicate variable", url: "https://example.com/$foo/$foo", wantErr: ErrDuplicateVariable},
		{name: "duplicate among three", url: "https://example.com/$foo/$bar/$foo", wantErr: ErrDuplicateVariable},
		{name: "duplicate in query", url: "https://example.com/?a=$x&b=$x", wantErr: ErrDuplicateVariable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateURLVariables(tt.url)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("ValidateURLVariables(%q) = %v, want nil", tt.url, err)
				}
				return
			}
			if err == nil {
				t.Errorf("ValidateURLVariables(%q) = nil, want %v", tt.url, tt.wantErr)
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidateURLVariables(%q) = %v, want error wrapping %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

// Governing: SPEC-0002 REQ "Links Table" scenarios "Title/Description Exceeds Maximum Length"
func TestValidateLinkText(t *testing.T) {
	tests := []struct {
		name        string
		title       string
		description string
		wantErr     error
	}{
		{name: "both empty", title: "", description: "", wantErr: nil},
		{name: "short values", title: "Docs", description: "The team docs", wantErr: nil},
		{name: "title at limit", title: strings.Repeat("a", MaxTitleLength), description: "", wantErr: nil},
		{name: "description at limit", title: "", description: strings.Repeat("b", MaxDescriptionLength), wantErr: nil},
		{name: "title over limit", title: strings.Repeat("a", MaxTitleLength+1), description: "", wantErr: ErrTitleTooLong},
		{name: "description over limit", title: "", description: strings.Repeat("b", MaxDescriptionLength+1), wantErr: ErrDescriptionTooLong},
		// Length is counted in runes, not bytes: 200 multi-byte chars are valid.
		{name: "multibyte title at limit", title: strings.Repeat("é", MaxTitleLength), description: "", wantErr: nil},
		{name: "multibyte title over limit", title: strings.Repeat("é", MaxTitleLength+1), description: "", wantErr: ErrTitleTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLinkText(tt.title, tt.description)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("ValidateLinkText(...) = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidateLinkText(...) = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
