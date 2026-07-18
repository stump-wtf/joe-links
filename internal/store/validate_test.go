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

// Only http(s) destinations may be stored: javascript:/data:/etc URLs are
// stored-XSS primitives on any redirect surface (issue #265).
func TestValidateLinkURL(t *testing.T) {
	valid := []string{
		"https://example.com",
		"http://example.com",
		"https://example.com/path?q=1#frag",
		"https://example.com/$user/$repo",       // variable placeholders survive parsing
		"HTTPS://EXAMPLE.COM",                   // url.Parse lowercases the scheme
		"  https://example.com  ",               // surrounding whitespace is trimmed
		"https://bücher.example/straße",         // IDN host + non-ASCII path
		"https://example.com:8443/path",         // explicit port
		"http://192.168.4.10:3000/grafana",      // IP literal host + port
		"https://user:secret@example.com/gated", // userinfo survives parsing
		"https://example.com/a b/c",             // legacy shape: unencoded space in path
	}
	for _, u := range valid {
		if err := ValidateLinkURL(u); err != nil {
			t.Errorf("ValidateLinkURL(%q) = %v, want nil", u, err)
		}
	}

	invalid := []string{
		"",
		"javascript:alert(1)",
		"JaVaScRiPt:alert(1)",
		" javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"ftp://example.com/file",
		"//evil.example.com/path", // scheme-relative
		"example.com/no-scheme",
		"java\nscript:alert(1)", // control chars fail url.Parse
	}
	for _, u := range invalid {
		if err := ValidateLinkURL(u); !errors.Is(err, ErrURLSchemeInvalid) {
			t.Errorf("ValidateLinkURL(%q) = %v, want ErrURLSchemeInvalid", u, err)
		}
	}

	// A valid scheme with no host is unresolvable typo territory, not a
	// security issue — rejected with its own error so the message can say
	// what is actually wrong (PR #280 review follow-up).
	hostless := []string{
		"http://",
		"https:",
		"http:opaque",
		"https://?q=1",
		"https:///path-only",
	}
	for _, u := range hostless {
		if err := ValidateLinkURL(u); !errors.Is(err, ErrURLHostMissing) {
			t.Errorf("ValidateLinkURL(%q) = %v, want ErrURLHostMissing", u, err)
		}
	}
}

// Tag display names get a safe charset at intake — defense in depth for the
// stored XSS fixed at the output layer in #246, and a guarantee that every
// accepted name derives a non-empty slug (issue #251).
func TestValidateTagName(t *testing.T) {
	valid := []string{
		"jira",
		"Jira",
		"My Tag",
		"web-dev",
		"snake_case",
		"a",
		"3d-printing",
		"  padded  ", // matches store trimming
		strings.Repeat("a", MaxTagNameLength),
	}
	for _, name := range valid {
		if err := ValidateTagName(name); err != nil {
			t.Errorf("ValidateTagName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []struct {
		name    string
		tag     string
		wantErr error
	}{
		{"empty", "", ErrTagNameInvalid},
		{"whitespace only", "   ", ErrTagNameInvalid},
		{"XSS payload from #241 review", `x');fetch('/evil')//`, ErrTagNameInvalid},
		{"html injection", `<img src=x onerror=alert(1)>`, ErrTagNameInvalid},
		{"attribute breakout", `x') alert('pwned`, ErrTagNameInvalid},
		{"leading hyphen", "-foo", ErrTagNameInvalid},
		{"leading underscore", "_foo", ErrTagNameInvalid},
		{"empty-slug degenerate: non-ASCII", "日本語", ErrTagNameInvalid},
		{"empty-slug degenerate: symbols", `'';!--"<XSS>`, ErrTagNameInvalid},
		{"comma", "a,b", ErrTagNameInvalid},
		{"over max length", strings.Repeat("a", MaxTagNameLength+1), ErrTagNameTooLong},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateTagName(tt.tag); !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidateTagName(%q) = %v, want %v", tt.tag, err, tt.wantErr)
			}
		})
	}
}

// A single write may attach at most MaxTagsPerLink tags (issue #265).
func TestValidateTagNames(t *testing.T) {
	atCap := make([]string, MaxTagsPerLink)
	for i := range atCap {
		atCap[i] = "tag" + strings.Repeat("x", i%5)
	}
	if err := ValidateTagNames(atCap); err != nil {
		t.Errorf("ValidateTagNames(%d tags) = %v, want nil", len(atCap), err)
	}

	overCap := append(atCap, "one-too-many")
	if err := ValidateTagNames(overCap); !errors.Is(err, ErrTooManyTags) {
		t.Errorf("ValidateTagNames(%d tags) = %v, want ErrTooManyTags", len(overCap), err)
	}

	if err := ValidateTagNames([]string{"fine", `x');fetch('/evil')//`}); !errors.Is(err, ErrTagNameInvalid) {
		t.Errorf("ValidateTagNames with hostile name = %v, want ErrTagNameInvalid", err)
	}
	if err := ValidateTagNames(nil); err != nil {
		t.Errorf("ValidateTagNames(nil) = %v, want nil", err)
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
