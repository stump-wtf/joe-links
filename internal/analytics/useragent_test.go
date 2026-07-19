// Story #277 — read-time user-agent classification (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "Common UA classified"
//   - "Token ordering respected"
//   - "Hostile UA bounded"
//
// Governing: SPEC-0021 REQ "User-Agent Parsing", ADR-0021
package analytics_test

import (
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/analytics"
)

// Scenario: Common UA classified — a current Firefox-on-Windows string counts
// as browser "Firefox", OS "Windows". The table doubles as the spec-required
// fixture set of real-world UA strings per family.
// Governing: SPEC-0021 REQ "User-Agent Parsing"
func TestUserAgentParsing_CommonUAClassified(t *testing.T) {
	cases := []struct {
		name        string
		ua          string
		wantBrowser string
		wantOS      string
	}{
		{
			name:        "Firefox on Windows",
			ua:          "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0",
			wantBrowser: "Firefox",
			wantOS:      "Windows",
		},
		{
			name:        "Firefox on Linux",
			ua:          "Mozilla/5.0 (X11; Linux x86_64; rv:126.0) Gecko/20100101 Firefox/126.0",
			wantBrowser: "Firefox",
			wantOS:      "Linux",
		},
		{
			name:        "Chrome on Windows",
			ua:          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
			wantBrowser: "Chrome",
			wantOS:      "Windows",
		},
		{
			name:        "Chrome on Android",
			ua:          "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36",
			wantBrowser: "Chrome",
			wantOS:      "Android",
		},
		{
			name:        "Safari on macOS",
			ua:          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15",
			wantBrowser: "Safari",
			wantOS:      "macOS",
		},
		{
			name:        "Safari on iPhone",
			ua:          "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1",
			wantBrowser: "Safari",
			wantOS:      "iOS",
		},
		{
			name:        "Edge on Windows",
			ua:          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36 Edg/125.0.0.0",
			wantBrowser: "Edge",
			wantOS:      "Windows",
		},
		{
			name:        "curl",
			ua:          "curl/8.6.0",
			wantBrowser: "Bot/CLI",
			wantOS:      "Other",
		},
		{
			name:        "wget",
			ua:          "Wget/1.21.4",
			wantBrowser: "Bot/CLI",
			wantOS:      "Other",
		},
		{
			name:        "Googlebot",
			ua:          "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Googlebot/2.1; +http://www.google.com/bot.html) Chrome/125.0.0.0 Safari/537.36",
			wantBrowser: "Bot/CLI",
			wantOS:      "Other",
		},
		{
			name:        "generic spider",
			ua:          "ExampleSpider/1.0 (+https://example.com/spider)",
			wantBrowser: "Bot/CLI",
			wantOS:      "Other",
		},
		{
			name:        "generic crawler",
			ua:          "SomeCrawler/2.0",
			wantBrowser: "Bot/CLI",
			wantOS:      "Other",
		},
		{
			name:        "unmatched UA is Other, never a new category",
			ua:          "TotallyNovelBrowser/1.0 (UnheardOfOS 9000)",
			wantBrowser: "Other",
			wantOS:      "Other",
		},
		{
			name:        "empty UA is Unknown",
			ua:          "",
			wantBrowser: "Unknown",
			wantOS:      "Unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			browser, os := analytics.ClassifyUA(tc.ua)
			if browser != tc.wantBrowser {
				t.Errorf("browser = %q, want %q (ua: %s)", browser, tc.wantBrowser, tc.ua)
			}
			if os != tc.wantOS {
				t.Errorf("os = %q, want %q (ua: %s)", os, tc.wantOS, tc.ua)
			}
		})
	}
}

// Scenario: Token ordering respected — a UA containing both Chrome/ and Edg/
// tokens (Edge does) classifies as "Edge", not "Chrome". Also pins the other
// normative orderings: Chrome before Safari, iOS before macOS ("like Mac OS
// X" in iPhone UAs), Android before Linux.
// Governing: SPEC-0021 REQ "User-Agent Parsing"
func TestUserAgentParsing_TokenOrderingRespected(t *testing.T) {
	// Edge before Chrome: real Edge UAs carry Chrome/, Safari/, and Edg/.
	edgeUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36 Edg/125.0.0.0"
	if browser, _ := analytics.ClassifyUA(edgeUA); browser != "Edge" {
		t.Errorf("Edge UA classified as %q, want Edge (Edge before Chrome)", browser)
	}

	// Chrome before Safari: Chrome UAs carry Safari/.
	chromeUA := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
	if browser, _ := analytics.ClassifyUA(chromeUA); browser != "Chrome" {
		t.Errorf("Chrome UA classified as %q, want Chrome (Chrome before Safari)", browser)
	}

	// iOS before macOS: iPhone UAs claim "like Mac OS X".
	iphoneUA := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1"
	if _, os := analytics.ClassifyUA(iphoneUA); os != "iOS" {
		t.Errorf("iPhone UA OS classified as %q, want iOS (iOS before macOS)", os)
	}

	// Android before Linux: Android UAs claim Linux.
	androidUA := "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36"
	if _, os := analytics.ClassifyUA(androidUA); os != "Android" {
		t.Errorf("Android UA OS classified as %q, want Android (Android before Linux)", os)
	}
}

// Scenario: Hostile UA bounded — 512 runes of adversarial repeated tokens
// classify in a single bounded pass and return a family or "Other": no
// backtracking, no unbounded work, and input beyond the 512-rune bound is
// ignored entirely.
// Governing: SPEC-0021 REQ "User-Agent Parsing", Security Requirements "UA Parsing DoS"
func TestUserAgentParsing_HostileUABounded(t *testing.T) {
	closedBrowsers := map[string]bool{
		"Firefox": true, "Edge": true, "Chrome": true, "Safari": true,
		"Bot/CLI": true, "Other": true, "Unknown": true,
	}
	closedOS := map[string]bool{
		"Windows": true, "iOS": true, "Android": true, "macOS": true,
		"Linux": true, "Other": true, "Unknown": true,
	}

	hostile := []string{
		// Near-miss token prefixes repeated to the cap — the classic
		// backtracking trigger for regex-based parsers.
		strings.Repeat("chrchrchrchr", 43)[:512],
		strings.Repeat("cUrLcUrLbOt-", 43)[:512],
		strings.Repeat("firefo", 86)[:512],
		strings.Repeat("=", 512),
		strings.Repeat("\x00", 512),
		strings.Repeat("safarisafari", 43)[:512],
		strings.Repeat("🦊", 512), // multi-byte runes at the bound
	}
	for i, ua := range hostile {
		browser, os := analytics.ClassifyUA(ua)
		if !closedBrowsers[browser] {
			t.Errorf("hostile[%d]: browser %q outside the closed taxonomy", i, browser)
		}
		if !closedOS[os] {
			t.Errorf("hostile[%d]: os %q outside the closed taxonomy", i, os)
		}
	}

	// The parser enforces its own input bound: a token that only appears
	// after rune 512 must not influence classification.
	overlong := strings.Repeat("a", 512) + " Firefox/127.0 Windows"
	browser, os := analytics.ClassifyUA(overlong)
	if browser != "Other" || os != "Other" {
		t.Errorf("overlong UA classified as (%q, %q), want (Other, Other): input beyond 512 runes must be ignored", browser, os)
	}

	// The bound is rune-aware: 512 multi-byte runes followed by a token.
	overlongRunes := strings.Repeat("é", 512) + "curl"
	if b, _ := analytics.ClassifyUA(overlongRunes); b != "Other" {
		t.Errorf("multi-byte overlong UA classified as %q, want Other", b)
	}
}
