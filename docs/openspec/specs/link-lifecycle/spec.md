---
status: draft
date: 2026-07-17
implements: [ADR-0020]
requires: [SPEC-0002, SPEC-0004, SPEC-0005, SPEC-0006, SPEC-0007, SPEC-0009, SPEC-0010, SPEC-0011, SPEC-0012, SPEC-0014, SPEC-0016, SPEC-0018, SPEC-0019]
---

# SPEC-0020: Link Lifecycle — Expiration, Archiving, and Dead-Link Detection

## Overview

This specification defines the link lifecycle layer for joe-links (epic #217): optional expiration via `expires_at`, an owner-facing archive toggle distinct from delete, a polite periodic dead-link checker with per-link opt-out, staleness views over existing click data, and lifecycle state in REST and MCP responses. Per ADR-0020, lifecycle state is **derived** from two nullable timestamps (`archived` when `archived_at` is set, else `expired` when `expires_at <= now`, else `active`) — there is no status column — and the resolver evaluates **visibility (SPEC-0010) first, lifecycle second**, so no lifecycle surface can disclose the existence of a secure slug to a viewer the visibility model would not already inform. Webhooks and event delivery are explicitly out of scope for v1.

See ADR-0020 (Link Lifecycle), SPEC-0002 (Link Data Model), SPEC-0004 (Application Views and Routing — the 404 page), SPEC-0005 (REST API Layer), SPEC-0010 (Link Visibility Modes), SPEC-0012 (Public Link Browsing), SPEC-0016 (Link Analytics — click data and cascade-on-delete), SPEC-0018 (MCP parity), SPEC-0019 (discovery surfaces).

---

## Requirements

### Requirement: Link Expiration

The `links` table MUST gain a nullable `expires_at` timestamp column (UTC) via a goose migration (next free number in `internal/db/migrations/`, starting at `00016` — per-story migrations are acceptable, see References), added as plain portable SQL with no per-dialect branching (ADR-0002 / ADR-0020). `NULL` means the link never expires and MUST be the default for existing and new links. A link is **expired** when `expires_at` is non-null and `expires_at <= now` (server UTC); expiry MUST be derived at read time — no background job, no stored status value.

The link create and edit forms (SPEC-0004) MUST offer an optional expiration date/time input, empty by default, clearable on edit. `POST /api/v1/links` and `PUT /api/v1/links/{id}` MUST accept an optional nullable `expires_at` field (RFC 3339); omitting it on create MUST yield `NULL`, and explicitly passing `null` on update MUST clear it. Setting `expires_at` to a **new** past value MUST be rejected with a validation error on both create and update (immediate disabling is what archiving is for); an update whose `expires_at` equals the stored value — including a round-tripped past value on an already-expired link, which the edit form ("clearable on edit" renders the stored value) and any full-resource `PUT` naturally send — is not a change and MUST be accepted, so expired links remain editable without first renewing. Setting or clearing `expires_at` MUST be authorized by `LinkCaps.CanEdit` (owners, co-owners, admins — SPEC-0002 REQ "Authorization Based on Ownership"); share recipients MUST NOT be able to modify it.

#### Scenario: Link Created with Expiration

- **WHEN** a user creates a link with `expires_at` set to a future timestamp
- **THEN** the link MUST be persisted with that `expires_at` and MUST resolve normally until that time is reached

#### Scenario: Past Expiration Rejected

- **WHEN** a create or update request sets `expires_at` to a past timestamp that differs from the stored value
- **THEN** the application MUST return a validation error and MUST NOT persist the change

#### Scenario: Expired Link Stays Editable

- **WHEN** an owner edits the title or destination of an expired link and the request round-trips the stored (past) `expires_at` unchanged
- **THEN** the update MUST succeed and `expires_at` MUST remain the stored value

#### Scenario: Expiration Cleared on Edit

- **WHEN** an owner edits a link and clears the expiration field (or `PUT`s `"expires_at": null`)
- **THEN** the stored `expires_at` MUST become `NULL` and the link MUST no longer expire

#### Scenario: Share Recipient Cannot Set Expiry

- **WHEN** a user whose only relationship to a link is a `link_shares` record attempts to set or clear `expires_at`
- **THEN** the server MUST respond `403 Forbidden` and MUST NOT modify the link

---

### Requirement: Expired Link Resolution

When the resolver reaches a link whose derived state is expired, it MUST first have applied the link's visibility rules (see Security Requirements — "Resolution Ordering"): for a `secure` link, unauthenticated visitors MUST still be redirected to login and unauthorized users MUST still receive `403 Forbidden` exactly as for an active secure link, with no indication of expiry. Only after the visibility gate passes MAY the lifecycle outcome be rendered.

An expired link MUST NOT redirect. The server MUST respond **`404 Not Found`** (never `410 Gone` — ADR-0020) with a styled "this link has expired" page that names the requested slug. The page MUST name the link's owner (display name, linking to the owner's public profile per SPEC-0012) **only when** the link's visibility is `public` or the viewer holds `CanView` on the link; for other viewers of a `private` expired link the page MUST omit owner identity. The page MUST NOT offer the 404 page's "Create it now" CTA — the slug remains reserved by the expired link. No click event (SPEC-0016) may be recorded for an expired resolution. When requested with the `HX-Request` header, the same content MUST be rendered as a fragment (SPEC-0004 conventions).

Prefix resolution (ADR-0013 / SPEC-0009): when a multi-segment path's first visibility-passing prefix match is expired or archived, the resolver MUST commit to that match — the same `404`/expired-page (or archived-404) outcome as an exact match, naming the matched slug rather than the full requested path — and MUST NOT fall through to shorter prefixes. This preserves today's stop-at-first-visible-match semantics.

Owners, co-owners, admins, and share recipients MUST continue to see expired links on the dashboard, link detail, and stats surfaces they could already access, with an "expired" badge (see REQ "Health Badges and Admin Report" for badge visibility rules).

#### Scenario: Expired Public Link Shows Expired Page

- **WHEN** any visitor requests `/{slug}` for a public link whose `expires_at` is in the past
- **THEN** the server MUST respond `404 Not Found` with the expired page naming the slug and the owner, MUST NOT redirect, and MUST NOT record a click

#### Scenario: Expired Secure Link — Anonymous Visitor Learns Nothing

- **WHEN** an unauthenticated visitor requests `/{slug}` for an expired `secure` link
- **THEN** the server MUST redirect to `/auth/login?return_url=/{slug}` exactly as for an active secure link, and MUST NOT render the expired page

#### Scenario: Expired Secure Link — Authorized Viewer Sees Expiry

- **WHEN** an owner or share recipient of an expired `secure` link requests `/{slug}`
- **THEN** the server MUST respond `404 Not Found` with the expired page (owner MAY be named — the viewer holds `CanView`)

#### Scenario: Expired Prefix Match Terminates Resolution

- **WHEN** a visitor requests `/jira/PROJ-1` and the visibility-passing prefix link `jira` is expired
- **THEN** the resolver MUST commit to the `jira` match and respond `404 Not Found` with the expired page naming the slug `jira`, without falling through to shorter prefixes

#### Scenario: Owner Sees Expired Badge on Dashboard

- **WHEN** an owner views their dashboard and one of their links is expired
- **THEN** the link row MUST appear with an "expired" badge and a renew action

---

### Requirement: Archive State

The `links` table MUST gain a nullable `archived_at` timestamp column (UTC) in the same migration series as `expires_at` (the same portable-SQL constraints; a separate per-story migration is acceptable). A link is **archived** when `archived_at` is non-null. Archiving MUST be a reversible toggle exposed on the link detail/edit surfaces (`POST /dashboard/links/{id}/archive` and `POST /dashboard/links/{id}/unarchive`, HTMX-aware) and via the REST API (`PUT /api/v1/links/{id}` accepting a boolean `archived` field; `true` sets `archived_at = now` if not already set, `false` clears it). Archive and unarchive MUST be authorized by `LinkCaps.CanEdit`.

Archiving MUST be distinct from deletion in all of the following ways: the `links` row is retained; the slug remains globally reserved (SPEC-0002 slug uniqueness — creating a new link with an archived link's slug MUST fail with the existing slug-taken error); all `link_clicks` rows are retained (deletion cascades them per SPEC-0016 — archiving MUST NOT); and stats pages remain accessible to viewers with `CanStats`. `archived_at` and `expires_at` are independent facts: a link may hold both, and the derived state MUST report `archived` when both apply.

#### Scenario: Archive Toggle Stops Resolution, Keeps Stats

- **WHEN** an owner archives a link that has recorded clicks
- **THEN** the link MUST stop resolving, its `link_clicks` rows MUST be unchanged, and its stats page MUST remain accessible to the owner

#### Scenario: Archived Slug Stays Reserved

- **WHEN** any user attempts to create a new link with the slug of an archived link
- **THEN** creation MUST fail with the slug-taken validation error

#### Scenario: Unarchive Restores Resolution

- **WHEN** an owner unarchives a link (and the link is not expired)
- **THEN** `archived_at` MUST be cleared and `/{slug}` MUST resolve with `302 Found` again

#### Scenario: Non-Editor Cannot Archive

- **WHEN** a share recipient or unrelated user attempts the archive or unarchive action
- **THEN** the server MUST respond `403 Forbidden`

---

### Requirement: Archived Link Resolution

When the resolver reaches a link whose derived state is archived — after the visibility gate, under the same ordering as expired links — it MUST respond **`404 Not Found`** rendering the standard 404 page (SPEC-0004), not a distinct "archived" page: archive means the link should present as gone. The rendered 404 page MUST NOT offer the "Create it now" CTA for the requested slug (the slug is reserved). Secure-link visibility behavior MUST be identical to the expired case: anonymous → login redirect, unauthorized → `403`, with no archived-state disclosure. No click event may be recorded.

#### Scenario: Archived Public Link Renders 404 Without Create CTA

- **WHEN** any visitor requests `/{slug}` for an archived public link
- **THEN** the server MUST respond `404 Not Found` with the standard 404 page, without the Create CTA, and MUST NOT record a click

#### Scenario: Archived Secure Link — Anonymous Visitor Learns Nothing

- **WHEN** an unauthenticated visitor requests `/{slug}` for an archived `secure` link
- **THEN** the server MUST redirect to `/auth/login?return_url=/{slug}` exactly as for an active secure link

#### Scenario: Archived Beats Expired in Derived State

- **WHEN** a link has both `archived_at` set and `expires_at` in the past
- **THEN** its derived lifecycle state MUST be `archived` and resolution MUST follow the archived behavior

---

### Requirement: Renewal

Expired links MUST offer a one-click renew action to viewers with `CanEdit`: `POST /dashboard/links/{id}/renew` MUST clear `expires_at` (making the link permanent until a new expiry is set) and re-render the affected row or detail view via HTMX. Setting a *new* future expiry is performed through the ordinary edit form and `PUT /api/v1/links/{id}` — the REST API needs no dedicated renew endpoint; parity is achieved via `"expires_at": null` on update (ADR-0020). The renew action MUST be idempotent (renewing a non-expired link with an `expires_at` simply clears it; renewing a link with no `expires_at` is a no-op) and MUST NOT touch `archived_at`.

#### Scenario: One-Click Renew Clears Expiry

- **WHEN** an owner clicks "Renew" on an expired link
- **THEN** `expires_at` MUST become `NULL`, the link MUST resolve again (absent archive), and the row MUST re-render without the expired badge

#### Scenario: Renew Requires Edit Capability

- **WHEN** a share recipient invokes the renew action on an expired link shared with them
- **THEN** the server MUST respond `403 Forbidden` and the link MUST remain expired

#### Scenario: Renew Does Not Unarchive

- **WHEN** a link is both archived and expired and an owner renews it
- **THEN** `expires_at` MUST be cleared but the link MUST remain archived and MUST NOT resolve

---

### Requirement: Destination Health Checking

The server MUST run an in-process destination health checker as a background goroutine started by `joe-links serve` (the `cmd/joe-links/serve.go` pattern established by the click writer and gauge updater — ADR-0016 / ADR-0020), tied to the server's shutdown context. Configuration MUST be viper-loaded with the `JOE_` prefix: `JOE_HEALTH_CHECKS_ENABLED` (default `true`), `JOE_HEALTH_CHECK_INTERVAL` (default `24h`, minimum enforced `1h`), `JOE_HEALTH_CHECK_TIMEOUT` (default `10s`, maximum enforced `30s`), `JOE_HEALTH_CHECK_ALLOW_PRIVATE` (default `false` — see Security Requirements). When disabled, the goroutine MUST NOT start and no health data is written. The checker assumes a single-instance topology (ADR-0020); multi-replica deployments SHOULD set `JOE_HEALTH_CHECKS_ENABLED=false` on all but one instance so destinations are not probed once per replica.

**Eligibility.** Each cycle MUST consider only links that are due (`next_check_at` absent or `<= now`) and MUST skip: archived links, expired links, links with `health_checks_disabled = TRUE` (the per-link opt-out, a boolean column on `links` defaulting to `FALSE`, editable with `CanEdit` via the link form and API), and variable links (SPEC-0009 — a URL template is not a fetchable destination). No fetch may be issued for a skipped link, and skipped links MUST NOT be marked broken; a `link_health` row belonging to a link that later becomes archived, expired, or opted out simply freezes and stops being surfaced (see **State** below).

**Probe semantics.** Each probe MUST issue `HEAD` first and fall back to `GET` if the destination answers `405` or `501` (the resolver's own #196 lesson, applied outbound). Probes MUST use the configured timeout, send an identifying `User-Agent` (`joe-links-health/<version>`), follow at most 5 redirects, and read at most 64 KiB of any `GET` body before discarding. A final `2xx` or `3xx` status is a success; `4xx`, `5xx`, network errors, timeouts, and redirect-limit exhaustion are failures — **except** `429 Too Many Requests`, which MUST NOT count as a failure and MUST push the link's next check out by at least one full interval (honoring `Retry-After` when present and larger); a `429` MUST neither increment nor reset `consecutive_failures` — the counter is unchanged. Known false-positive class (accepted): destinations that block bots — WAFs and auth walls answering `401`/`403` to the identifying User-Agent — will be reported broken after three strikes even though they work in a browser; the per-link `health_checks_disabled` opt-out is the remedy. The checker deliberately does not consult robots.txt (a stated decision — it is a low-frequency health probe, not a crawler).

**Politeness.** The checker MUST NOT hammer: at most 4 probes in flight at once, at least 1 second between successive probes to the same host, and per-link exponential backoff on failure — after `n` consecutive failures the next check MUST be scheduled no sooner than `interval × 2^(n−1)`, capped at `7 × interval`. Healthy links are checked once per interval. A link is **broken** when `consecutive_failures >= 3`; the broken state MUST be derived from the failure counter, not stored separately.

**State.** Health state MUST be persisted in a dedicated `link_health` table created in the same migration series — one row per checked link: `link_id` (PK, FK → `links.id` ON DELETE CASCADE), `last_checked_at`, `last_status` (nullable HTTP status), `last_error` (nullable text), `consecutive_failures` (NOT NULL DEFAULT 0), `next_check_at`, and `skipped` (BOOLEAN NOT NULL DEFAULT FALSE — set when the most recent probe attempt was skipped because the destination's scheme or resolved address is not checkable under current policy, cleared by any completed probe). The derived health state is: `skipped` when `skipped` is true; else `broken` when `consecutive_failures >= 3`; else `ok` when a row exists; `unchecked` when no row exists. **Surfacing rule:** health state MUST be surfaced (badges, REST, MCP) only while the link is eligible for checking — when a link is opted out (`health_checks_disabled = TRUE`), archived, or expired, its frozen `link_health` row MUST NOT be surfaced: the API/MCP `health.status` MUST report `"unchecked"` with `last_checked_at` and `last_status` null, and no health badge renders. The checker MUST NOT write to the `links` table (no `updated_at` churn — ADR-0020). Absence of a row means "never checked".

#### Scenario: Healthy Destination

- **WHEN** the checker probes a link whose destination answers `HEAD` with `200`
- **THEN** the `link_health` row MUST record the status and time with `consecutive_failures = 0`, and the next check MUST be scheduled one interval out

#### Scenario: HEAD Rejected, GET Fallback

- **WHEN** a destination answers `HEAD` with `405 Method Not Allowed` but `GET` with `200`
- **THEN** the probe MUST be recorded as a success

#### Scenario: Failures Back Off and Eventually Mark Broken

- **WHEN** a destination fails three consecutive checks (the gap after each failure growing per the `interval × 2^(n−1)` backoff schedule)
- **THEN** the link MUST be reported broken, and its next check MUST have been scheduled with exponentially increasing gaps (capped at 7 × interval) rather than retried within the same cycle

#### Scenario: 429 Is Not a Failure

- **WHEN** a destination answers `429` with `Retry-After: 172800`
- **THEN** `consecutive_failures` MUST NOT change (neither increment nor reset) and the next check MUST be at least 48 hours out

#### Scenario: Opt-Out Honored

- **WHEN** an owner sets the health-check opt-out on a link
- **THEN** the checker MUST NOT probe that destination, its health badge MUST disappear, and REST/MCP `health.status` MUST report `"unchecked"` (the frozen `link_health` row is no longer surfaced)

#### Scenario: Checker Disabled by Config

- **WHEN** the server runs with `JOE_HEALTH_CHECKS_ENABLED=false`
- **THEN** no probes are issued and no `link_health` rows are written

---

### Requirement: Health Badges and Admin Report

Dashboard link rows and the link detail page MUST show a "broken" badge for links whose derived health state is broken, alongside the "expired" and "archived" lifecycle badges; a broken badge MUST NOT render on an archived or expired link — its frozen health row is not surfaced (REQ "Destination Health Checking", surfacing rule), so only the lifecycle badge appears. Badge visibility MUST respect SPEC-0010 and the capability model: lifecycle and health badges are rendered only on rows the viewer can already see under SPEC-0010's list filtering, and health information (broken badge, last status, last checked) MUST be shown only to viewers holding capabilities on the link (owners, co-owners, admins, share recipients). Public surfaces — the public link browser and profile pages (SPEC-0012) — MUST NOT display health information; additionally, expired and archived links MUST be excluded from public browsing, from the suggest endpoint, and from 404 did-you-mean candidates (SPEC-0019) for all callers, since suggesting a link that will not resolve is worse than no suggestion. These exclusions are normative carve-outs from SPEC-0012 (whose current text mandates the public browser display **all** `visibility = 'public'` links) and SPEC-0019 (whose candidate-set REQs enumerate visibility filters only); on conflict this spec wins — see "Deferred Reciprocal Amendments". All list surfaces MUST render these badges consistently per SPEC-0014.

Admins MUST have a report of failing links at `/admin/link-health` (SPEC-0011 conventions): all currently broken links with slug, destination URL, owner(s), last HTTP status or error, last checked time, and consecutive-failure count, ordered by most failures first. The report MUST link each row to the link's detail page.

#### Scenario: Broken Badge on Owner Dashboard

- **WHEN** an owner's link has 3 or more consecutive failed checks
- **THEN** the owner's dashboard row for that link MUST show a "broken" badge

#### Scenario: Public Browser Shows No Health Data

- **WHEN** an anonymous visitor browses public links
- **THEN** no health badges or check data are rendered, and expired/archived links do not appear at all

#### Scenario: Admin Report Lists Failing Links

- **WHEN** an admin visits `/admin/link-health` while two links are broken
- **THEN** both links MUST be listed with status, owner, and last-checked details; healthy, opted-out, skipped, and never-checked links MUST NOT be listed

---

### Requirement: Staleness Views

The dashboard MUST provide filters, over the viewer's own SPEC-0010 dashboard scope, for: **"stale"** — links created more than 90 days ago with no recorded click (SPEC-0016 `link_clicks`) in the last 90 days — and **"never clicked"** — links created more than 7 days ago with no recorded click at all. Both computations MUST derive from `link_clicks` at query time in the store layer (no denormalized counters in v1) and MUST treat archived links as out of scope (archiving is a deliberate retirement, not staleness). The admin links view (SPEC-0011) SHOULD offer the same filters across all links. Because these views read historical click rows, any future click-retention pruning (epic #216) MUST keep its retention window at least as long as the 90-day staleness window, or this requirement must be revised to use a persisted last-clicked rollup — this coupling is recorded in ADR-0020.

#### Scenario: Stale Filter

- **WHEN** an owner applies the "stale" filter and has a 6-month-old link whose last click was 4 months ago
- **THEN** that link MUST appear in the filtered list; a link clicked yesterday MUST NOT

#### Scenario: Never-Clicked Filter

- **WHEN** an owner applies the "never clicked" filter and has a 3-week-old link with zero clicks and a 2-day-old link with zero clicks
- **THEN** the 3-week-old link MUST appear and the 2-day-old link MUST NOT (creation grace period)

#### Scenario: Staleness Respects Visibility Scope

- **WHEN** a non-admin user applies a staleness filter
- **THEN** the results MUST contain only links already visible to them on the dashboard under SPEC-0010

---

### Requirement: Lifecycle State in API and MCP

All link resources returned by the REST API (SPEC-0005) MUST include: `expires_at` (RFC 3339 or `null`), `archived_at` (RFC 3339 or `null`), and `lifecycle_state` (derived: `"active"`, `"expired"`, or `"archived"` — archived wins when both apply). Link resources returned to callers holding capabilities on the link MUST additionally include a `health` object (`status` of `"unchecked"`, `"ok"`, `"broken"`, or `"skipped"`; `last_checked_at`; `last_status`) and the `health_checks_disabled` flag — subject to the surfacing rule in REQ "Destination Health Checking": for opted-out, archived, or expired links `status` MUST be `"unchecked"` with `last_checked_at` and `last_status` null; callers without capabilities MUST NOT receive the `health` object. `POST /api/v1/links` and `PUT /api/v1/links/{id}` MUST accept `expires_at` (nullable), `archived` (boolean, update only), and `health_checks_disabled` (boolean) under the same `CanEdit` authorization as the web UI. Swagger annotations MUST be updated and regenerated via `make swagger` (SPEC-0007).

The MCP surface (SPEC-0018) MUST have full parity per its REQ "Authorization Parity with the REST API": `get_link`/`list_links` responses MUST carry the same lifecycle and health fields with the same capability gating, and `update_link` MUST accept the same lifecycle inputs with identical validation (past `expires_at` rejected, `CanEdit` enforced). All lifecycle behavior MUST be implemented in the shared store layer so web, REST, and MCP cannot diverge.

**Non-goal:** the API MUST NOT emit webhooks, callbacks, or any push notification of lifecycle transitions in v1 (ADR-0020); lifecycle is observable state on resources only.

#### Scenario: API Response Carries Lifecycle State

- **WHEN** `GET /api/v1/links/{id}` is called by an owner for a link with `expires_at` in the past
- **THEN** the response MUST include the stored `expires_at`, `"archived_at": null`, and `"lifecycle_state": "expired"`

#### Scenario: MCP Parity

- **WHEN** the MCP `get_link` tool is invoked with a PAT whose user owns an archived link
- **THEN** the tool result MUST report `"lifecycle_state": "archived"` and the same `health` object the REST API would return

#### Scenario: Non-Capable Caller Gets No Health Data

- **WHEN** an authenticated API caller with no ownership, share, or admin relationship retrieves a public link
- **THEN** the response MUST include the lifecycle fields but MUST NOT include the `health` object

#### Scenario: API Archive Round-Trip

- **WHEN** an owner calls `PUT /api/v1/links/{id}` with `{"archived": true}` and later with `{"archived": false}`
- **THEN** the first call MUST set `archived_at` and the second MUST clear it, with `lifecycle_state` tracking accordingly

---

## Security Requirements

### Authentication

All endpoints MUST require authentication by default. Public (unauthenticated) endpoints MUST be explicitly listed with justification.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| GET /{slug} (expired/archived rendering) | Public | The resolver is inherently public (SPEC-0004); lifecycle output is gated by the ordering rule below |
| POST /dashboard/links/{id}/archive, /unarchive, /renew | Session + `CanEdit` | Lifecycle writes are edits (ADR-0020) |
| PUT /api/v1/links/{id} (lifecycle fields) | Bearer PAT (SPEC-0006) + `CanEdit` | — |
| GET /admin/link-health | Session + admin role | Health report spans all users' links |
| MCP tools (lifecycle fields) | Bearer PAT (SPEC-0018) + `CanEdit`/capabilities | — |

### Resolution Ordering and Oracle Resistance

The resolver MUST evaluate SPEC-0010 visibility rules **before** any lifecycle check, for every path (exact match, prefix/variable resolution, and HEAD requests): a `secure` link MUST produce the login redirect (anonymous) or `403` (unauthorized) regardless of expiry or archival, byte-identical to the active-link responses, so that neither page content, status code, nor redirect target discloses that the slug exists or has a lifecycle. This ordering is the load-bearing control — reversing it would turn the expired page into an existence oracle for secure slugs. For `public` and `private` links the expired/archived rendering discloses prior existence only to a viewer already presenting the slug, which SPEC-0010 already permits to resolve; no new information is exposed. Both lifecycle outcomes use HTTP `404` (never `410`) so the status-code surface cannot distinguish "never existed" from "existed once" for machines. Expired/archived links MUST be excluded from all SPEC-0019 discovery surfaces and SPEC-0012 public browsing, so lifecycle states cannot be enumerated. Under prefix resolution (ADR-0013 / SPEC-0009) the resolver commits to the first visibility-passing prefix match even when it is expired or archived (REQ "Expired Link Resolution") — the visibility gate still runs per-match, so the ordering rule holds at every prefix depth.

Accepted existence signal (Create CTA differential): because the Create CTA is suppressed for slugs reserved by expired/archived links, an anonymous visitor can distinguish a reserved-but-dead `public`/`private` slug (404 without CTA) from a free slug (404 with CTA). This is accepted within SPEC-0010's model — private is a discoverability control, not an access control, and a viewer presenting the slug is already permitted to resolve it; `secure` slugs are protected by the ordering rule above and never reach CTA logic. Modulo the CTA and the expired page's specified content, the expired and archived responses MUST be byte-identical to the generic 404 (no differing titles or owner strings beyond what this spec mandates) so the differential does not widen.

### SSRF Resistance (Health Checker)

The health checker is the first component that makes the server fetch user-supplied URLs (destination URLs are not scheme- or host-validated today — `internal/store/validate.go` — and MUST NOT be retroactively restricted for resolution, which only redirects browsers). The checker MUST therefore contain the fetch itself:

- Probes MUST use `http`/`https` only; any other scheme is recorded as **skipped** (derived health state `skipped` — REQ "Destination Health Checking").
- By default (`JOE_HEALTH_CHECK_ALLOW_PRIVATE=false`) the checker MUST apply a **deny-by-default IP classifier** to every resolved address of every connection it makes. Before any range test, IPv4-mapped and IPv4-compatible IPv6 addresses MUST be unmapped to their IPv4 form (Go `To4()`), so `::ffff:127.0.0.1` classifies as loopback rather than slipping past IPv6-only range checks. The classifier MUST refuse: loopback, private (RFC 1918), link-local unicast and multicast (`169.254.0.0/16` — including cloud metadata endpoints — and `fe80::/10`), unique-local (`fc00::/7`), CGNAT (`100.64.0.0/10`), "this network" (`0.0.0.0/8`), unspecified (`0.0.0.0`, `::`), broadcast (`255.255.255.255`), multicast, and NAT64-mapped (`64:ff9b::/96`) addresses. It MUST be built on the standard library's address-class predicates (`IsLoopback`, `IsPrivate`, `IsLinkLocalUnicast`, `IsLinkLocalMulticast`, `IsUnspecified`, `IsMulticast`) plus the explicit CGNAT/this-network/NAT64 ranges — not a hand-maintained CIDR literal list, which is itself the vulnerability. Enforcement MUST occur at dial time against the actual resolved IP (connection-control hook), not by pre-resolving hostnames, so DNS rebinding cannot bypass it; every redirect hop MUST be re-checked.
- All probe traffic — the initial `HEAD`, the `GET` fallback, every retry, and every redirect hop — MUST go through one shared SSRF-guarded `http.Transport` whose dialer carries the classifier, so no request path can bypass the guard; each connection is independently gated. A redirect to a non-`http(s)` scheme MUST NOT be followed: it terminates the probe and is recorded as a normal terminal response, not a failure.
- Blocked destinations MUST be recorded as **skipped, never broken** (unchecked is not dead), and the owner-facing UI SHOULD indicate that the destination is not checkable under current server policy.
- `JOE_HEALTH_CHECK_ALLOW_PRIVATE=true` is a global, operator-level opt-in for homelab deployments that shortlink internal services (ADR-0020); there MUST NOT be a per-link private-fetch override, since link creation is not an operator privilege.
- Probe responses MUST NOT be stored beyond status code and a bounded error string (no response bodies persisted), and `GET` fallback reads MUST be size-capped.

### Rate Limiting and Abuse

The checker's outbound behavior is itself the rate-limit surface: the politeness constraints in REQ "Destination Health Checking" (bounded concurrency, per-host spacing, backoff, `429` deference, interval floor of 1h) are normative and MUST NOT be relaxed by configuration. Inbound, lifecycle endpoints follow the existing `/api/v1` posture (no application-level rate limiting in v1, per SPEC-0018/SPEC-0019 precedent).

### Output Escaping

Owner display names on the expired page, destination URLs and error strings on the admin health report, and all badge/tooltip content MUST be rendered through the HTML template engine's escaping (`html/template`); checker error strings are attacker-influenced (a malicious destination controls response data) and MUST be treated as untrusted text everywhere they render.

---

## Deferred Reciprocal Amendments

This spec intentionally narrows normative statements in three accepted specs. The carve-outs are normative here — on conflict, this spec wins — and each older spec owes a reciprocal one-line amendment, deferred to the docs consolidation PR, which MUST apply all three:

- **SPEC-0012 (User Profiles and Public Link Browsing).** SPEC-0012 currently mandates that the public browser display **all** links with `visibility = 'public'` and that profile pages list a user's public links. Carve-out: expired and archived links MUST be excluded from both surfaces (REQ "Health Badges and Admin Report"; Security "Resolution Ordering"). Deferred amendment: qualify both SPEC-0012 sentences with "excluding expired and archived links (SPEC-0020)".
- **SPEC-0004 (Application Views and Routing).** SPEC-0004 unconditionally mandates the "Create it now" CTA on the 404 page. Carve-out: the CTA MUST be suppressed for slugs reserved by expired or archived links (REQ "Expired Link Resolution", REQ "Archived Link Resolution"). Deferred amendment: except lifecycle-reserved slugs in SPEC-0004's REQ "Slug Resolver and 404 Page".
- **SPEC-0019 (Search & Discovery).** SPEC-0019's candidate-set REQs enumerate visibility filters only. Carve-out: the suggest endpoint and 404 did-you-mean candidates MUST additionally exclude expired and archived links for all callers (REQ "Health Badges and Admin Report"). Deferred amendment: add the lifecycle predicate to SPEC-0019's candidate-set REQs alongside the visibility filters.

---

## References

- ADR-0020 (Link Lifecycle — implemented by this spec); epic issue #217
- SPEC-0002 (Link Data Model — `links` table gaining `expires_at`, `archived_at`, `health_checks_disabled`; slug uniqueness that keeps archived slugs reserved; `LinkCaps` authorization)
- SPEC-0004 (Application Views and Routing — the 404 page the archived state reuses and the expired page sits beside; Create CTA suppression)
- SPEC-0005 / SPEC-0007 (REST API layer and swagger regeneration for the new fields)
- SPEC-0006 (API Token Authentication — bearer PATs gating the lifecycle API writes)
- SPEC-0009 (URL Variable Substitution — variable links are skipped by the checker)
- SPEC-0010 (Link Visibility Modes — the gate that MUST run before lifecycle; capability sourcing)
- SPEC-0011 (Admin Management — conventions for `/admin/link-health`)
- SPEC-0012 (Public Link Browsing — expired/archived exclusion)
- SPEC-0014 (Link List UI Consistency — badges uniform across list surfaces)
- SPEC-0016 (Link Analytics — `link_clicks` powering staleness views; delete-cascades-clicks contrast with archive; the click-writer goroutine pattern the checker follows). Note the coupling with epic #216: a future click-retention window shorter than 90 days would silently falsify the "stale" filter — see REQ "Staleness Views".
- SPEC-0018 (MCP — parity obligations for lifecycle and health fields)
- SPEC-0019 (Search & Discovery — suggest and did-you-mean surfaces that MUST exclude expired/archived links)
- Issue #196 (resolver HEAD support — the outbound HEAD→GET fallback mirrors it)
- Migration requirement: one or more goose migrations (next free numbers in `internal/db/migrations/`, starting at `00016` — each implementation story MAY carry its own migration) add the three `links` columns and create `link_health`; every migration MUST be plain portable SQL across sqlite3/mysql/postgres (ADR-0002, ADR-0020 — no 00015-style per-dialect Go migration), and each down migration MUST drop what its up migration added.
