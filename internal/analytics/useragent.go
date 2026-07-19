// Package analytics holds the read-time analytics computations for the v2
// analytics layer: bounded user-agent classification into browser and OS
// families. Classification happens at read time — no parsed columns exist in
// the database and no external UA-parsing dependency is used — so family-table
// improvements retroactively reclassify all stored history (ADR-0021).
//
// Governing: SPEC-0021 REQ "User-Agent Parsing", ADR-0021
package analytics

import "strings"

// MaxUARunes is the parser's own input bound. Capture already rune-truncates
// user_agent to 512 (SPEC-0016), but the parser enforces the bound again so it
// is safe against any caller — the bound is structural, not a timeout.
// Governing: SPEC-0021 REQ "User-Agent Parsing" — Scenario Hostile UA bounded
const MaxUARunes = 512

// The closed output taxonomy (SPEC-0021): classification yields exactly the
// family names in the match tables below plus these two — unrecognized tokens
// MUST NOT mint new categories.
const (
	// FamilyOther is the classification for a non-empty UA matching no token.
	FamilyOther = "Other"
	// FamilyUnknown is the classification for an empty/null UA.
	FamilyUnknown = "Unknown"
)

// uaFamily is one row of an ordered match table: a family name and the
// lowercase tokens that claim it. Matching is ordered case-insensitive
// substring search — no regular expressions, no backtracking (ADR-0021).
type uaFamily struct {
	name   string
	tokens []string
}

// browserFamilies is the normative ordered browser match table — first match
// wins. Orderings that MUST hold (SPEC-0021): Edge before Chrome (Edge UAs
// carry both Edg/ and Chrome/ tokens) and Chrome before Safari (Chrome UAs
// carry Safari). Bot/CLI is checked first because bot UAs routinely embed
// browser tokens (Googlebot claims both Chrome and Safari).
// Governing: SPEC-0021 REQ "User-Agent Parsing" — Scenario Token ordering respected
var browserFamilies = []uaFamily{
	{name: "Bot/CLI", tokens: []string{"curl", "wget", "bot", "spider", "crawler", "python-requests", "go-http-client"}},
	{name: "Edge", tokens: []string{"edg"}}, // Edge/, Edg/, EdgA/, EdgiOS/
	{name: "Firefox", tokens: []string{"firefox", "fxios"}},
	{name: "Chrome", tokens: []string{"chrome", "crios", "chromium"}},
	{name: "Safari", tokens: []string{"safari"}},
}

// osFamilies is the normative ordered OS match table — first match wins.
// Orderings that MUST hold (SPEC-0021): iOS before macOS (iPhone/iPad UAs
// claim "like Mac OS X") and Android before Linux (Android UAs claim Linux).
// Governing: SPEC-0021 REQ "User-Agent Parsing" — Scenario Token ordering respected
var osFamilies = []uaFamily{
	{name: "Windows", tokens: []string{"windows"}},
	{name: "iOS", tokens: []string{"iphone", "ipad", "ipod", "crios", "fxios"}},
	{name: "Android", tokens: []string{"android"}},
	{name: "macOS", tokens: []string{"macintosh", "mac os x"}},
	{name: "Linux", tokens: []string{"linux", "x11"}},
}

// ClassifyUA maps a stored user_agent value to (browser, os) families using
// only ordered case-insensitive substring matching against the fixed token
// tables above. The work is bounded by construction: input is capped at
// MaxUARunes, lowercased in one pass, and each of the fixed number of tokens
// costs at most one linear substring scan — hostile or adversarial UA strings
// cost no more than any other 512-rune input. An empty UA classifies as
// Unknown; a non-empty UA matching nothing classifies as Other.
// Governing: SPEC-0021 REQ "User-Agent Parsing", ADR-0021
func ClassifyUA(ua string) (browser, os string) {
	if ua == "" {
		return FamilyUnknown, FamilyUnknown
	}
	// Rune-aware bound: never split a multi-byte character.
	if runes := []rune(ua); len(runes) > MaxUARunes {
		ua = string(runes[:MaxUARunes])
	}
	lower := strings.ToLower(ua)
	return matchFamily(lower, browserFamilies), matchFamily(lower, osFamilies)
}

// matchFamily returns the first family whose token list matches lower, or
// FamilyOther when nothing matches (the taxonomy is closed).
func matchFamily(lower string, families []uaFamily) string {
	for _, f := range families {
		for _, tok := range f.tokens {
			if strings.Contains(lower, tok) {
				return f.name
			}
		}
	}
	return FamilyOther
}
