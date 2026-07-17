---
status: draft
date: 2026-07-17
implements: [ADR-0019]
requires: [SPEC-0002, SPEC-0004, SPEC-0005, SPEC-0006, SPEC-0007, SPEC-0008, SPEC-0009, SPEC-0010]
---

# SPEC-0019: Search & Discovery

## Overview

This specification defines the search and discovery layer for joe-links (epic #218): case-insensitive slug resolution, a lightweight autocomplete suggest endpoint at `GET /api/v1/links/suggest`, "Did you mean go/…?" suggestions on the 404 page, omnibox integration in the browser extension, and optional slug-normalization forgiveness. Per ADR-0019, all matching uses portable SQL (`LIKE` only — no trigram or FTS extensions, per ADR-0002) plus in-Go ranking and bounded Levenshtein distance, and **every discovery surface is visibility-filtered in the shared store layer** so that suggestions can never reveal a slug the viewer could not already discover under SPEC-0010.

See ADR-0019 (Search & Discovery), SPEC-0004 (Application Views and Routing — the 404 page this extends), SPEC-0005 (REST API Layer), SPEC-0006 (PAT bearer auth), SPEC-0008 (Browser Extension), SPEC-0009 (URL Variable Substitution), SPEC-0010 (Link Visibility Modes), SPEC-0017 (the unrelated `POST /api/v1/links/suggest` LLM metadata operation sharing the path).

---

## Requirements

### Requirement: Case-Insensitive Slug Resolution

The slug resolver (SPEC-0004 REQ "Slug Resolver and 404 Page") MUST resolve slugs case-insensitively. Because `ValidateSlugFormat` (SPEC-0002) rejects uppercase characters, all stored slugs are canonically lowercase; therefore the resolver MUST lowercase each candidate slug or slug-prefix before calling `GetBySlug`, and MUST NOT require any database collation or schema change. Exact-case handling needs no tie-breaking rule: a canonical lowercase row is the only possible match.

Case-folding MUST apply uniformly to exact-match lookup and to every prefix candidate during multi-segment resolution (SPEC-0009 REQ "Multi-Segment Path Resolution"). Path segments consumed as variable values MUST be passed to substitution with their original case preserved — only the slug-prefix portion of the path is lowercased. Resolution precedence MUST be unchanged: keyword host routing and path-based keyword routing (ADR-0011, SPEC-0008) are evaluated before slug resolution exactly as today, and reserved route prefixes (SPEC-0004 REQ "Route Registration and Priority") keep precedence over the resolver. Slug creation and update validation MUST be unchanged: uppercase slugs remain rejected, keeping the stored corpus canonical.

#### Scenario: Mixed-Case Slug Resolves

- **WHEN** a request arrives at `/JIRA` and a link with slug `jira` exists
- **THEN** the server MUST apply the link's visibility rules (SPEC-0010) and, when permitted, respond `302 Found` to the stored URL

#### Scenario: Mixed-Case Prefix with Variable Link Preserves Argument Case

- **WHEN** a request arrives at `/Jira/PROJ-123` and a link with slug `jira` has URL `https://example.atlassian.net/browse/$ticket`
- **THEN** the resolver MUST match the prefix `jira` case-insensitively and substitute `$ticket` with `PROJ-123` exactly as received (case preserved)

#### Scenario: Keyword Routing Precedence Unchanged

- **WHEN** a request arrives whose host or first path segment matches a registered keyword (SPEC-0008)
- **THEN** keyword routing MUST handle the request before any slug lookup, exactly as before this spec

#### Scenario: Uppercase Slug Still Rejected at Creation

- **WHEN** a link is created or updated with slug `Jira`
- **THEN** validation MUST reject it with the existing slug-format error; case-insensitivity is a resolution behavior only

---

### Requirement: Suggest Endpoint

The REST API MUST provide `GET /api/v1/links/suggest?q={query}` returning autocomplete suggestions for links. The endpoint MUST require `Authorization: Bearer` PAT authentication exactly like all `/api/v1` routes; per SPEC-0006 REQ "No Web UI Session on API Routes", session cookies MUST NOT be accepted and unauthenticated requests MUST receive `401 Unauthorized`. This `GET` operation is distinct from SPEC-0017's `POST /api/v1/links/suggest` (LLM metadata generation); both MUST be documented with unambiguous swagger summaries and the endpoint MUST carry swaggo annotations regenerated via `make swagger` (SPEC-0007).

The response MUST be a JSON object with a `"suggestions"` array whose entries contain at minimum `slug` and `title` (title MAY be empty). Matching MUST use only portable SQL (`LIKE` with escaped user input — `%` and `_` in `q` MUST be treated literally) per ADR-0019. The server MUST lowercase `q` before matching and compare it against the canonical lowercase slug corpus (SPEC-0002), so matching behaves identically on sqlite3, mysql, and postgres (postgres `LIKE` is case-sensitive). Ranking MUST order results: (1) slug-prefix matches, (2) slug-substring matches, (3) title/description matches; within each band, results MUST be ordered by slug ascending (byte order), and band order takes precedence across bands. Result count MUST be capped: default 5, maximum 10 via an optional `limit` parameter; a `limit` above 10 MUST be clamped to 10, and a non-numeric `limit` MUST be rejected with `400`. The query MUST be bounded: an empty `q` MUST return an empty suggestions array, and `q` longer than 64 characters MUST be truncated to 64. Matching against title/description MUST be case-insensitive.

Visibility filtering MUST be enforced in the shared store layer (never in the handler): non-admin callers MUST receive only links they own or co-own, links shared with the viewer via `link_shares`, and `public` links; other users' `private` and `secure` links MUST NOT appear. Admin callers receive all links (SPEC-0010 REQ "Admin Visibility Override"). Matching MUST be performed only against fields the caller is authorized to read under SPEC-0010, so that a future field addition cannot turn matching into a content-probing oracle for data the caller could not read directly.

#### Scenario: Prefix Match Ranks First

- **WHEN** an authenticated user calls `GET /api/v1/links/suggest?q=ji` and visible links `jira` (slug-prefix), `fiji-trip` (slug-substring), and `docs` titled "Jira runbook" exist
- **THEN** the response lists `jira` before `fiji-trip` before `docs`, and contains at most 5 entries

#### Scenario: Other Users' Private Links Excluded

- **WHEN** user A calls the suggest endpoint with a query matching user B's `private` link slug
- **THEN** the response MUST NOT contain that link

#### Scenario: Shared Secure Link Included

- **WHEN** a user with a `link_shares` record for a `secure` link queries a prefix of its slug
- **THEN** that link MUST appear in the suggestions

#### Scenario: Unauthenticated Request Rejected

- **WHEN** `GET /api/v1/links/suggest?q=ji` is called without an `Authorization` header (with or without a valid session cookie)
- **THEN** the server MUST respond `401 Unauthorized` and reveal no suggestion data

#### Scenario: LIKE Wildcards Neutralized

- **WHEN** the query is `%` or `_`
- **THEN** the characters MUST be matched literally, not as SQL wildcards

---

### Requirement: Did-You-Mean 404 Suggestions

When the resolver cannot resolve a requested path, the 404 page (SPEC-0004 REQ "Slug Resolver and 404 Page") MUST render up to 3 "Did you mean `{keyword}/{slug}`?" suggestions above the existing "Create it now" CTA, each linking to `/{slug}` so the resolver's own visibility enforcement (SPEC-0010) governs the actual redirect. `{keyword}` is the configured short keyword (`JOE_SHORT_KEYWORD`, defaulting to the first DNS label of the server hostname) — the same value the server advertises via `GET /api/v1/config` (SPEC-0005 / SPEC-0008). When no candidate qualifies, the 404 page MUST render exactly as it does today; the Create CTA and its slug pre-fill MUST be unchanged in all cases.

Candidates MUST be computed in Go per ADR-0019: plain Levenshtein edit distance between the lowercased requested path and each candidate slug, considering only candidates whose length is within ±2 of the request, admitting only distance ≤ 2, and ordering by ascending distance, with ties within each distance broken by slug ascending (byte order). Only the first path segment of the requested path is matched. Did-you-mean MUST NOT be computed for an empty or single-character requested path (the resolver renders the 404 page with an empty slug for the bare root path). No cache or precomputed index is used.

The candidate slug set MUST be visibility-filtered in the shared store layer to slugs *discoverable by the viewer*: anonymous viewers MUST only be offered `public` link slugs — never `private` or `secure` slugs, whose existence MUST NOT be leaked even though private links would redirect if the exact slug were known (discoverability, not resolvability, is the governing test). Authenticated non-admin viewers MUST be offered `public` slugs plus their own/co-owned links of any visibility plus links shared with the viewer via `link_shares`. Admins MAY be offered any slug. Rendered slugs MUST be HTML-escaped. When the 404 is served as an HTMX fragment (`HX-Request`), the fragment MUST include the same did-you-mean block as the full page.

#### Scenario: Typo Suggests Nearby Public Slug

- **WHEN** an anonymous user requests `/jria` and a public link `jira` exists
- **THEN** the 404 page MUST show "Did you mean `go/jira`?" (at most 3 suggestions) above the Create CTA, linking to `/jira`

#### Scenario: Private Slug Existence Not Leaked to Anonymous Viewer

- **WHEN** an anonymous user requests `/secrt` and the only slug within distance 2 is another user's `private` link `secret`
- **THEN** the 404 page MUST render with no did-you-mean suggestions

#### Scenario: Owner Sees Their Own Private Slug Suggested

- **WHEN** an authenticated user requests `/secrt` and owns a `private` link `secret`
- **THEN** "Did you mean `go/secret`?" MUST be rendered

#### Scenario: Distance Bound Enforced

- **WHEN** a user requests `/zzzzz` and no visible slug is within Levenshtein distance 2
- **THEN** the 404 page MUST render without a did-you-mean block, with the Create CTA unchanged

#### Scenario: HTMX Fragment Includes Suggestions

- **WHEN** an unresolvable path is requested with the `HX-Request` header and qualifying candidates exist
- **THEN** the rendered fragment MUST include the did-you-mean suggestions

---

### Requirement: Extension Omnibox Integration

The browser extension (SPEC-0008) MUST register an `omnibox` keyword in `manifest.json` so users can type `{keyword} {text}` in the address bar. All omnibox API usage MUST be feature-detected (e.g. `typeof chrome.omnibox !== 'undefined'`): the shared background script is referenced directly by the Safari Web Extension Xcode project (SPEC-0015) and Safari provides no omnibox API, so the API's absence MUST NOT throw or affect any other extension behavior — search interception in particular. On omnibox input changes the extension MUST call `GET {baseURL}/api/v1/links/suggest?q={text}` with the stored PAT as `Authorization: Bearer` (SPEC-0008 REQ "API Key Authentication"), percent-encoding the typed text when building the query string, debounced so that rapid keystrokes coalesce (debounce interval ≥ 150 ms), and MUST surface at most 5 suggestions showing the slug and, when present, the title. Suggestion description strings MUST be XML-escaped per the omnibox API contract. The short-link prefix shown in suggestion text SHOULD use the server's advertised keyword from `GET /api/v1/config` (SPEC-0008 REQ "Keyword Host Discovery").

Selecting a suggestion (or pressing Enter on free text) MUST navigate the tab to the resolver URL `{baseURL}/{slug}` — never directly to a destination URL — so server-side visibility enforcement (SPEC-0010) always applies. The navigation URL MUST be constructed by joining the percent-encoded slug or typed text onto the configured base URL as a path segment, so the result is always same-origin with `{baseURL}`; the typed text MUST never be interpreted as an absolute URL (no `new URL(text)` on user input). When no PAT is configured, the omnibox MUST NOT issue suggest requests; it MUST degrade gracefully by offering only a default suggestion (e.g. prompting the user to configure the extension, or navigating the raw text to the resolver on Enter). Suggest request failures (network error, 401) MUST fail silently with no suggestions and MUST NOT break Enter-to-navigate.

#### Scenario: Omnibox Suggestions Appear

- **WHEN** a user with a configured base URL and PAT types the omnibox keyword, a space, and `ji`
- **THEN** the extension queries the suggest endpoint (debounced) and displays up to 5 slug+title suggestions

#### Scenario: Suggestion Selection Navigates via Resolver

- **WHEN** the user selects the suggestion for slug `jira`
- **THEN** the tab navigates to `{baseURL}/jira` and the server performs the redirect

#### Scenario: Debounce Coalesces Keystrokes

- **WHEN** the user types five characters in quick succession
- **THEN** the extension issues at most one suggest request after input settles, not five

#### Scenario: No PAT Configured

- **WHEN** no API key is saved in the extension options and the user types in the omnibox
- **THEN** no request is sent to the suggest endpoint and a default suggestion is shown; pressing Enter still navigates the typed text to `{baseURL}/{text}`

#### Scenario: Browser Without Omnibox API

- **WHEN** the extension runs in a browser that does not provide the omnibox API (e.g. Safari, whose Xcode project references the same background script — SPEC-0015)
- **THEN** no error is thrown during background-script startup and search interception and all other extension behavior keep working

---

### Requirement: Slug Normalization Forgiveness

The resolver MAY apply normalization forgiveness when an exact (case-folded) lookup fails, before falling through to prefix matching and the 404 path: retrying with underscores (`_`) replaced by hyphens (`-`), and retrying with trailing punctuation (`.`, `,`, `;`, `:`, `!`, `?`, `)`) stripped — as happens when a `go/slug` link is pasted at the end of a sentence. If implemented, normalization MUST apply only to resolution lookups — never to slug creation, update, or uniqueness checks — and normalized matches MUST pass through the same visibility enforcement (SPEC-0010) as exact matches. Normalization retries MUST be attempted before did-you-mean suggestions are computed.

#### Scenario: Underscore Forgiven

- **WHEN** a request arrives at `/standup_notes` and a link with slug `standup-notes` exists
- **THEN** the resolver MAY resolve it as `standup-notes`, subject to the link's visibility rules

#### Scenario: Trailing Punctuation Forgiven

- **WHEN** a request arrives at `/jira.` and a link with slug `jira` exists
- **THEN** the resolver MAY strip the trailing `.` and resolve `jira`

---

## Security Requirements

### Authentication

All endpoints MUST require authentication by default. Public (unauthenticated) endpoints MUST be explicitly listed with justification.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| GET /api/v1/links/suggest | Required (Bearer PAT only, per SPEC-0006) | — |
| GET /{slug} (404 did-you-mean rendering) | Public | The resolver and its 404 page are inherently public (SPEC-0004); anonymous did-you-mean output is restricted to `public` link slugs |

### Enumeration Resistance

Discovery surfaces MUST NOT become slug-enumeration oracles beyond what browsing already permits: the suggest endpoint's scope for a non-admin equals their SPEC-0010 browsing scope plus public links (already enumerable via SPEC-0012's public browser), and the anonymous 404 path exposes only public slugs. Response timing differences between "no match" and "match withheld for visibility" SHOULD be negligible (both paths execute the same filtered query).

### Rate Limiting

No application-level rate limiting in v1, matching the `/api/v1` posture recorded in SPEC-0018: the suggest endpoint is PAT-gated and result-capped, and 404 did-you-mean work is bounded (±2 length pre-filter, distance ≤ 2, ≤ 3 results). If the REST API gains rate limiting, the suggest endpoint MUST adopt the same limits.

### Output Escaping

Did-you-mean slugs rendered into HTML MUST be escaped by the template engine; omnibox suggestion descriptions MUST be XML-escaped. User-controlled query text MUST never be interpolated into SQL (parameterized queries with `LIKE`-wildcard escaping) — the stored-XSS-in-autocomplete bug fixed in #248 is the precedent this requirement guards against.

---

## References

- ADR-0019 (Search & Discovery — implemented by this spec); epic issue #218
- SPEC-0002 (Link Data Model — `ValidateSlugFormat` guarantees the canonically lowercase slug corpus that case-insensitive resolution and lowercased-`q` matching rely on)
- SPEC-0004 (Application Views and Routing — 404 page extended by did-you-mean; its "search bar to find similarly-named links" remains and is complemented, not replaced)
- SPEC-0005 / ADR-0008 (REST API layer — router mounting, error format, pagination conventions the suggest endpoint deliberately omits)
- SPEC-0006 / ADR-0009 (PAT bearer auth — no-session rule governing the suggest endpoint)
- SPEC-0008 / ADR-0012 (browser extension — PAT storage, config discovery, packaging for the omnibox surface)
- SPEC-0009 / ADR-0013 (variable substitution — case preservation of variable segments, prefix-resolution order)
- SPEC-0010 / ADR-0014 (visibility modes — the filter bounding every surface in this spec)
- SPEC-0012 (public link browsing — the baseline for what anonymous/public discovery already exposes)
- SPEC-0015 (Safari Web Extension — its Xcode project references the shared extension sources, which is why omnibox API usage is feature-detected)
- SPEC-0017 / ADR-0017 (LLM metadata suggestions — the `POST` operation sharing `/api/v1/links/suggest`)
- SPEC-0007 (OpenAPI/Swagger — annotation and regeneration obligations)
