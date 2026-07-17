---
status: draft
date: 2026-07-17
implements: [ADR-0021]
requires: [SPEC-0002, SPEC-0004, SPEC-0005, SPEC-0006, SPEC-0007, SPEC-0010, SPEC-0016]
---

# SPEC-0021: Link Analytics v2 — Time Series, Breakdowns, Exports, Retention

## Overview

This specification defines the v2 analytics read layer for joe-links (epic #216): a per-link daily-clicks time series (30/90 days) rendered as a server-side SVG chart, a global analytics dashboard at `/dashboard/analytics`, referrer/browser/OS/authentication breakdowns, viewer-local timezone rendering of click timestamps, streaming CSV export, an opt-in click-retention policy enforced by a periodic pruner, and REST API parity for every new surface. See ADR-0021 for the architectural decisions.

**Relationship to SPEC-0016**: this spec `requires` SPEC-0016 rather than replacing it. The v1 capture pipeline (click recording, `link_clicks` schema, IP hashing, referrer/UA truncation), the Prometheus baseline, the v1 stats page contents, and the v1 `/stats` and `/clicks` endpoints remain normative as SPEC-0016 states them; v2 is additive read surfaces over the same rows. Two scoped amendments are explicit:

1. **Access control**: SPEC-0016's stats-access wording predates PR #255 — it still grants the stats page and stats/clicks endpoints to owners, co-owners, and admins only, with 403 for everyone else. This spec normatively states the current rule and **supersedes that wording**: the `LinkCaps` capability matrix governs — `CanStats` (owners, co-owners, admins, **and share recipients**) gates all stats/clicks reads, and clicker attribution is `CanManageShares`-only. Where SPEC-0016's owner-only/403-recipient text conflicts with the Capability Gating table below, this spec wins.
2. **Retention**: the Click Retention requirement below amends SPEC-0016's implicit keep-forever property — an amendment ADR-0016 pre-approved ("a background goroutine … can purge rows older than a configurable retention window. This is additive").

**Authorization baseline**: every surface in this spec is gated through the `LinkCaps` capability matrix (`internal/store/auth.go`, PR #255): `CanStats` (owners, co-owners, admins, and share recipients) gates all per-link analytics reads; `CanManageShares` (owners, co-owners, admins — "managers") exclusively gates clicker attribution, because the authenticated clickers of a `secure` link proxy its hidden share roster. The Capability Gating requirement pins each surface to the matrix.

The MCP tool surface (SPEC-0018) is unchanged by this spec: its v1 tool inventory is closed, so exposing v2 data through `get_link_stats` is deferred to a SPEC-0018 revision.

---

## Requirements

### Requirement: Per-Link Daily Time Series

The per-link stats page (`GET /dashboard/links/{id}/stats`, SPEC-0016 REQ "Link Stats Dashboard Page") MUST additionally display a daily-clicks chart covering a selectable window of the last 30 days (default) or 90 days. The chart MUST be rendered server-side as inline SVG from `html/template` — no chart library and no client-side rendering JavaScript (ADR-0021). The window toggle MUST be an HTMX fragment swap re-requesting the chart partial with a `days` parameter; `days` values other than `30` and `90` MUST be rejected with `400` (API) or fall back to 30 (web UI).

The series MUST be computed on the fly per ADR-0021: a single indexed range query selecting `clicked_at` for the link within the window, bucketed by **UTC calendar day** in Go. Window boundaries are pinned: buckets are whole UTC calendar days; the window is today's UTC day (the newest bucket, partial until the day ends) plus the preceding `days − 1` whole days; and the SQL range predicate is aligned to the UTC midnight opening the oldest bucket — never a rolling `now − 30d` bound. No SQL date functions (`strftime`, `DATE_FORMAT`, `to_char`, `date_trunc`) may appear in any query issued by this feature (ADR-0002). The series MUST contain exactly one entry per day in the window, in ascending date order, with zero-count days present (gap-filled) — a chart consumer never interpolates missing days. Day boundaries are UTC even though timestamps elsewhere render viewer-local (see REQ "Viewer-Local Timestamps"); this is an accepted, documented simplification.

When retention (REQ "Click Retention") is enabled with a horizon shorter than the selected window, the chart MUST render days older than the horizon as **no-data** — visually distinct from zero-count days — because a pruned day is not an unclicked day. This applies to the 90-day window whenever the retention horizon is under 90 days.

Access MUST be gated by `CanStats` exactly as the enclosing stats page already is. The chart displays counts only and MUST NOT carry any per-user information.

Aggregate stats reads (series, breakdowns, dashboard panels) SHOULD complete within 500 ms of server time at a reference scale of 1,000,000 `link_clicks` rows; a deployment that cannot meet this is the trigger for the rollup-table escape hatch documented in ADR-0021 — which is a future schema change, not a v2 deliverable.

#### Scenario: Chart renders with gap days

- **WHEN** an owner opens the stats page for a link that was clicked on only 3 of the last 30 days
- **THEN** the SVG chart shows 30 day positions, with zero-height marks on the 27 unclicked days

#### Scenario: Window toggle swaps fragment

- **WHEN** the viewer activates the 90-day toggle
- **THEN** an HTMX request refetches the chart partial with `days=90` and the swapped fragment covers 90 gap-filled days

#### Scenario: Share recipient sees the chart

- **WHEN** a user whose only relationship to a `secure` link is a `link_shares` record opens the stats page
- **THEN** the daily chart renders (counts only, no user attribution anywhere in it)

#### Scenario: UTC day bucketing

- **WHEN** a click is recorded at `2026-07-16T23:30:00Z` and the viewer's browser is in UTC−05:00
- **THEN** the click counts toward the `2026-07-16` bucket (UTC day), regardless of the viewer's timezone

#### Scenario: Pruned days distinguished from zero days

- **WHEN** retention is enabled with a 60-day horizon and the viewer selects the 90-day window
- **THEN** the oldest 30 day positions render as no-data (before the retention horizon), visually distinct from zero-count days inside the horizon

---

### Requirement: Time Series API

`GET /api/v1/links/{id}/stats/timeseries` MUST return the daily series as JSON. Authentication and authorization are identical to `GET /api/v1/links/{id}/stats` (SPEC-0016): bearer PAT only (SPEC-0006), `404` for unknown links, `403` when the caller lacks `CanStats`. The optional `days` parameter accepts `30` (default) or `90`; any other value MUST return `400`.

The response body MUST conform to:

```json
{
  "link_id": "uuid",
  "days": 30,
  "series": [
    { "date": "2026-06-18", "count": 0 },
    { "date": "2026-06-19", "count": 12 }
  ]
}
```

`series` MUST contain exactly `days` entries, ascending by `date`, gap-filled with zeros; `date` is a UTC calendar day in `YYYY-MM-DD` form. The endpoint MUST carry swaggo annotations regenerated via `make swagger` (SPEC-0007) — and the same obligation applies to every endpoint added by this spec (note: the v1 stats/clicks endpoints are missing annotations, a gap recorded in PR #242's follow-ups; v2 MUST NOT extend it).

#### Scenario: Owner fetches series

- **WHEN** an owner calls `GET /api/v1/links/{id}/stats/timeseries?days=90` with a valid PAT
- **THEN** a 200 response contains exactly 90 ascending gap-filled entries

#### Scenario: Invalid window rejected

- **WHEN** a caller passes `days=7`
- **THEN** the server responds `400` with the standard error shape (SPEC-0005)

#### Scenario: Recipient authorized, stranger forbidden

- **WHEN** a share recipient calls the endpoint for the shared link, and an unrelated authenticated user calls it for the same link
- **THEN** the recipient receives 200 and the unrelated user receives 403

---

### Requirement: Click Breakdowns

The stats page MUST display, below the time series and over the same selectable 30/90-day window, three breakdown tables computed from the stored click rows:

1. **Referrers by host**: `referrer` values grouped by URL host (parsed in Go via `url.Parse`; scheme, path, query, and fragment discarded — capture already strips query and fragment per SPEC-0016, see Security Requirements). Rows with an empty or unparseable referrer group under "Direct / unknown". Top 10 hosts by count, descending, with remaining hosts summed into an "Other" row.
2. **Browser and OS**: families parsed from the stored `user_agent` at read time per REQ "User-Agent Parsing". Top entries descending with "Other" aggregation as above.
3. **Authenticated vs anonymous**: two counts — clicks with non-null `user_id` vs null (SPEC-0016's recording rule) — and their percentages.

All breakdowns are **counts only**. They MUST be visible to every caller with `CanStats`, including share recipients: breakdown aggregates carry no identities, so recipients get them in full — minus attribution, which no breakdown contains (ADR-0021). The auth-vs-anonymous split is roster-inert by construction: a `secure` link — the only place a hidden share roster exists — can never record an anonymous click, because SPEC-0010's resolution order sends unauthenticated requests to login before any redirect and SPEC-0016 records clicks only on successful redirects; the split is therefore degenerate (100% authenticated) exactly where roster inference would matter. Grouping and ranking MUST be computed in Go from portable column fetches; no SQL string functions on `referrer`/`user_agent`. All in-Go aggregation in this spec (day bucketing, breakdown grouping, dashboard panels) MUST stream over the result rows, accumulating counts in bounded memory — the fetched column values MUST NOT be materialized as a full in-memory slice (the same no-full-buffering rule the CSV export carries).

`GET /api/v1/links/{id}/stats/breakdowns` MUST return the same data as JSON, with `days` (30 default / 90) and the same auth/authorization as the timeseries endpoint:

```json
{
  "link_id": "uuid",
  "days": 30,
  "referrers": [ { "host": "news.ycombinator.com", "count": 40 } ],
  "browsers":  [ { "name": "Firefox", "count": 61 } ],
  "os":        [ { "name": "macOS", "count": 38 } ],
  "auth":      { "authenticated": 51, "anonymous": 72 }
}
```

#### Scenario: Referrers grouped by host

- **WHEN** clicks carry referrers `https://a.example/x` and `https://a.example/y?z=1` and one empty referrer
- **THEN** the referrer table shows `a.example: 2` and `Direct / unknown: 1`

#### Scenario: Recipient gets breakdowns without attribution

- **WHEN** a share recipient loads the stats page or calls the breakdowns endpoint
- **THEN** all three breakdowns are returned, and no field anywhere in them names or identifies any user

#### Scenario: Top-10 plus Other

- **WHEN** clicks in the window arrived from 14 distinct referrer hosts
- **THEN** the 10 largest hosts are listed and the remaining 4 are summed into a single "Other" row

---

### Requirement: User-Agent Parsing

Browser/OS classification MUST be performed at read time by a small internal Go parser (ADR-0021) — no external UA-parsing dependency and no parsed columns in the database. The parser MUST:

- Operate on the stored `user_agent` value, which SPEC-0016 already rune-truncates to 512 at capture; the parser MUST additionally enforce its own 512-rune input bound so it is safe against any caller.
- Use only ordered case-insensitive substring matching against a fixed known-token table (no regular expressions), first match wins. Minimum browser families: Firefox, Edge, Chrome, Safari, and a "Bot/CLI" family (minimum tokens: `curl`, `wget`, `bot`, `spider`, `crawler`); minimum OS families: Windows, macOS, iOS, Android, Linux. The taxonomy is **closed**: the output categories are exactly these families plus "Other" and "Unknown" — unrecognized tokens MUST NOT create new categories; everything unmatched MUST classify as "Other", and an empty/null UA MUST classify as "Unknown".
- Complete in a single linear pass per string — hostile or adversarial UA strings MUST cost no more than any other 512-rune input (see Security Requirements).

The parser's complete ordered token table — one fixed Go table — is the normative match-order artifact, with this spec's family and token lists as minimums. Orderings that MUST hold: Edge before Chrome, Chrome before Safari, iOS before macOS, since UA strings embed multiple tokens. Because parsing happens at read time, family-table improvements retroactively reclassify all history; tests MUST cover a fixture set of real-world UA strings per family.

#### Scenario: Common UA classified

- **WHEN** a click's UA is a current Firefox-on-Windows string
- **THEN** the breakdown counts it as browser "Firefox", OS "Windows"

#### Scenario: Token ordering respected

- **WHEN** a click's UA contains both `Chrome/` and `Edg/` tokens (Edge does)
- **THEN** it classifies as "Edge", not "Chrome"

#### Scenario: Hostile UA bounded

- **WHEN** a stored UA is 512 runes of adversarial repeated tokens
- **THEN** classification completes in a single linear pass and returns a family or "Other" — no backtracking, no unbounded work

---

### Requirement: Global Analytics Dashboard

`GET /dashboard/analytics` MUST render an authenticated dashboard page (unauthenticated requests redirect to login per SPEC-0004 conventions; HTMX fragment behavior per house pattern). Its default aggregation scope — the **personal scope** — MUST be exactly: links the viewer owns or co-owns **plus** links shared with the viewer via `link_shares`. For non-admins this enumeration equals their `CanStats` link set; for admins `CanStats` is universal, so the personal scope is deliberately narrower — admins reach instance-wide aggregates only via the explicit `scope=all` toggle below. The personal scope is defined by this enumeration, not by the `CanStats` predicate, which remains the exact gate for per-link surfaces. Other users' links — including their `public` links — MUST NOT contribute to any panel: `public` grants resolvability and browsability (SPEC-0010/SPEC-0012), not stats access, and this dashboard must not widen `CanStats` (ADR-0021). The scoped link-ID set MUST be resolved in the store layer, never in the handler.

The page MUST display, for a selectable period of the current week (default, last 7 days) or month (last 30 days):

1. **Top links**: the scope's most-clicked links in the period (top 10), each with slug, click count, and a **trend vs the previous equal-length period**: percentage change, or a "new" marker when the previous period count is zero and the current is non-zero.
2. **Never-clicked**: links in scope created at least 7 days ago with zero recorded clicks, newest first, capped at 10. When retention is enabled, the panel label MUST read "no clicks within retention" (see REQ "Click Retention").
3. **Busiest referrers**: top 10 referrer hosts across the scope's clicks in the period, grouped by host per REQ "Click Breakdowns".

The dashboard MUST render aggregate counts only — no clicker names or identities anywhere on the page. Admins default to the same personal scope; an explicit `scope=all` query parameter (with a visible UI toggle shown only to admins) switches to instance-wide aggregates. `scope=all` from a non-admin MUST be refused with `403` (API) / forbidden page (web), per SPEC-0010's admin-override style.

`GET /api/v1/analytics` MUST return the same panels as JSON (bearer PAT per SPEC-0006; `period=week|month`, default week; `scope=all` admin-only):

```json
{
  "period": "week",
  "scope": "mine",
  "top_links": [ { "link_id": "uuid", "slug": "jira", "count": 120, "previous_count": 80, "trend_pct": 50.0 } ],
  "never_clicked": [ { "link_id": "uuid", "slug": "old-doc", "created_at": "2026-06-01T00:00:00Z" } ],
  "top_referrers": [ { "host": "news.ycombinator.com", "count": 77 } ]
}
```

`trend_pct` MUST be `null` when `previous_count` is zero (the "new" case) — never `Infinity` or a division error.

#### Scenario: No cross-user leakage

- **WHEN** user A opens `/dashboard/analytics` and user B owns a heavily-clicked `public` link that A neither co-owns nor has a share for
- **THEN** B's link appears in none of A's panels and contributes to none of A's counts

#### Scenario: Shared link included for recipient

- **WHEN** a `secure` link is shared with user A and receives clicks this week
- **THEN** that link is eligible for A's top-links panel (counts only, no clicker identities)

#### Scenario: Admin toggle

- **WHEN** an admin requests `scope=all` and a non-admin requests `scope=all`
- **THEN** the admin receives instance-wide aggregates and the non-admin receives 403; without the parameter, both receive their personal scope

#### Scenario: Trend against previous period

- **WHEN** a link has 120 clicks this week and 80 the previous week, and another has 5 this week and 0 the previous week
- **THEN** the first shows +50% and the second shows the "new" marker (`trend_pct: null`)

---

### Requirement: Viewer-Local Timestamps

Every click timestamp rendered in the web UI (the v1 recent-clicks table and any v2 surface) MUST be emitted as `<time datetime="{RFC3339 UTC}">{UTC fallback text}</time>`. A single static vanilla-JavaScript snippet (no library, no shipped timezone data) MUST rewrite each such element's text to the viewer's local timezone using `Intl.DateTimeFormat`, set a `title` attribute preserving the UTC form, and re-run after HTMX swaps (`htmx:afterSwap`) so fragments render correctly. The snippet MUST make no network requests; its DOM writes MUST go through `textContent` (and `setAttribute` for `title`) only — never `innerHTML`; and no user-controlled string ever reaches the `datetime` attribute, which is server-stamped from `clicked_at`. With JavaScript unavailable, the UTC fallback text MUST remain — pure progressive enhancement (ADR-0021; no per-user timezone setting, no schema change). PR #257 (issue #206) landed an interim rendering of these timestamps — UTC-formatted cell text with a hardcoded " UTC" suffix and an RFC3339 `title` attribute — which this requirement supersedes: the implementation migrates that markup to the `<time datetime>` pattern (the UTC label lives inside the rewritten text and is replaced by the local rendering) rather than layering on top of it.

Machine-readable outputs are exempt by design: API JSON timestamps and CSV export timestamps MUST remain UTC RFC3339. Time-series day buckets remain UTC days per REQ "Per-Link Daily Time Series".

#### Scenario: Local rendering with UTC hover

- **WHEN** a click at `2026-07-17T14:00:00Z` is rendered for a viewer in UTC+01:00 with JS enabled
- **THEN** the cell text shows the 15:00 local time and its `title` shows the UTC timestamp

#### Scenario: No-JS fallback

- **WHEN** the same page is rendered with JavaScript disabled
- **THEN** the UTC text is displayed (current v1 behavior preserved)

#### Scenario: HTMX fragment localized

- **WHEN** the recent-clicks table is refreshed via an HTMX request
- **THEN** timestamps in the swapped fragment are localized after the swap

---

### Requirement: CSV Export

Click history MUST be exportable as CSV from two routes sharing one store-level streaming iterator and one encoder (ADR-0021):

- `GET /api/v1/links/{id}/stats/export` — bearer PAT only (SPEC-0006; session cookies rejected like all `/api/v1` routes).
- `GET /dashboard/links/{id}/stats/export` — session-authenticated, backing an "Export CSV" button on the stats page (SPEC-0006 forbids sessions on `/api/v1`, so the UI cannot call the API route; the paired route exists for exactly this reason).

Both routes MUST gate on `CanStats` (403 otherwise; 404 for unknown links; API errors in the SPEC-0005 shape). The response MUST stream (`Content-Type: text/csv; charset=utf-8`, `Content-Disposition: attachment; filename="{slug}-clicks.csv"`), reading the table in bounded keyset batches on `(clicked_at, id)` (the PR #242 pattern) — the full result set MUST NOT be buffered in memory.

The header row and column order MUST be exactly: `clicked_at,referrer,user_agent,browser,os,authenticated,user`.

- `clicked_at`: UTC RFC3339.
- `user_agent`: the stored raw value — populated **only** when the caller has `CanManageShares`; for all other callers the column MUST be present but empty in every row (same mechanism as `user`), because per-row device fingerprints correlated with exact timestamps are adjacent to attribution on `secure` links. The parsed `browser`/`os` family columns stay populated for every `CanStats` caller, so recipients lose nothing the breakdowns don't already give them.
- `browser`/`os`: read-time families per REQ "User-Agent Parsing" — populated for all `CanStats` callers.
- `authenticated`: `true`/`false` (non-null vs null `user_id`).
- `user`: the clicker's display name — populated **only** when the caller has `CanManageShares`; for all other callers (share recipients) the column MUST be present but empty in every row, keeping one stable schema with zero attribution leakage (PR #255 rule).

Rows MUST be ordered oldest-first by `(clicked_at, id)` — the same keyset order the iterator walks. Optional `from` and `to` RFC3339 parameters bound the window (invalid values → 400). A hard cap of **100,000 rows per response** MUST be enforced. Resumption uses an **opaque keyset cursor** (the SPEC-0016 clicks endpoint's PR #242 `(clicked_at, id)` pattern), never a bare timestamp — `clicked_at` has second precision on at least the mysql driver, so timestamp ties at the cap boundary are routine and a timestamp-only resume would silently skip or duplicate tied rows:

- An optional `cursor` parameter accepts an opaque token encoding the `(clicked_at, id)` position of the last exported row. Resumption is **exclusive** of that row: the next response begins at the strictly-next row in keyset order, with no row skipped and none duplicated. Malformed cursors → 400.
- When the cap truncates the export, the response MUST carry the continuation cursor in an **`X-Next-Cursor` response header**; the header's absence means the export is complete. The CSV body carries no metadata rows. Because response headers precede a streamed body, the server MUST determine truncation before streaming begins (a single keyset probe for the existence of a row beyond the cap) and set the header on the response head.
- Cursors are scoped to the link they were issued for (a keyset position is meaningless elsewhere) and carry no capability: every request — with or without a cursor — is re-authorized via `CanStats` for the requested link, so a cursor replayed against another link is just a position, still gated by that link's own check.

Every text-bearing cell MUST be CSV-injection-escaped per Security Requirements.

#### Scenario: Owner exports via UI button

- **WHEN** an owner clicks "Export CSV" on the stats page
- **THEN** the browser downloads a streamed CSV with the exact header above, rows oldest-first, and populated `user` and `user_agent` cells for authenticated clicks

#### Scenario: Recipient export has empty attribution columns

- **WHEN** a share recipient exports the same link (either route)
- **THEN** the CSV has the same seven columns, every `user` and `user_agent` cell is empty, and the `browser`/`os` cells remain populated

#### Scenario: Formula cell neutralized

- **WHEN** a stored referrer or UA value's first non-whitespace/control character is `=`, `+`, `-`, or `@`
- **THEN** the emitted cell is prefixed with `'` so spreadsheet applications treat it as text

#### Scenario: Cap and resume across a timestamp tie

- **WHEN** a link has 150,000 clicks, several rows share the `clicked_at` value of the 100,000th row, and the caller exports with no window
- **THEN** exactly 100,000 rows stream (oldest first) and the response carries an `X-Next-Cursor` header; a second request passing that cursor returns the remaining rows with no tied row skipped and none duplicated, and its response carries no `X-Next-Cursor`

#### Scenario: API route refuses session auth

- **WHEN** a browser with a valid session cookie but no bearer token requests the `/api/v1` export route
- **THEN** the server responds 401 (SPEC-0006)

---

### Requirement: Click Retention

A `JOE_CLICK_RETENTION` configuration value (viper, integer **days**; consistent with the `JOE_` conventions of SPEC-0001) MUST control click retention. **Unset or `0` MUST disable retention entirely — retention is off by default** (ADR-0021): v2 MUST NOT delete any click data on deployments that have not explicitly opted in. Negative or non-integer values MUST fail startup with a clear config error.

When enabled, a background pruner MUST periodically (at startup and at least every 24 hours thereafter) delete `link_clicks` rows with `clicked_at` older than `now - retention`. Deletion MUST use portable SQL in bounded batches (at most 10,000 rows per statement, iterating until done) so a large first prune cannot hold a long transaction on sqlite3. The batching pattern is pinned, the same way the SQL-date-function ban is: each batch MUST keyset-select the victim ids — `SELECT id FROM link_clicks WHERE clicked_at < ? ORDER BY clicked_at, id LIMIT 10000` — and then delete them with `DELETE FROM link_clicks WHERE id IN (…)`. `DELETE … LIMIT` MUST NOT be used in any form: postgres does not support it, sqlite3 requires a non-default build tag, and mysql rejects the same-table subselect workaround unless wrapped in a derived table — the select-ids-then-delete pattern is the only shape portable across all three drivers with no dialect hazard (ADR-0002). The pruner assumes a **single running joe-links instance** (mirroring SPEC-0020's forthcoming multi-replica clause): deployments running multiple replicas MUST disable retention — leave `JOE_CLICK_RETENTION` unset — on all but one instance. The pruner MUST log the number of rows pruned per run and increment a `joelinks_clicks_pruned_total` Prometheus counter (extending SPEC-0016's metrics registry; the no-`slug`-label cardinality rule applies).

**Retention ↔ SPEC-0020 staleness coupling (≥ 90 days)**: SPEC-0020's (forthcoming, epic #217) staleness views compute a 90-day window directly from `link_clicks`, and SPEC-0020/ADR-0020 record that click retention must not undercut it. This spec records the same constraint from its side so the cross-reference is symmetric: while staleness computes from `link_clicks`, `JOE_CLICK_RETENTION` MUST NOT be configured below **90 days** — once the staleness views ship, values `1–89` MUST fail startup with a config error naming this constraint — unless SPEC-0020 moves staleness onto a persisted rollup, which lifts the floor from its side.

**Irreversibility MUST be documented and surfaced**: pruned rows are unrecoverable — the README/config reference MUST state this in the variable's description, and the server MUST log a startup line stating retention is active and its horizon whenever the value is set. Pruning applies uniformly by click age to all links regardless of lifecycle state; whether archived links (epic #217) preserve summary stats beyond the horizon is owned by **SPEC-0020 (link lifecycle, forthcoming)** — this spec deliberately does not special-case archived links, and that coordination point is recorded here and in ADR-0021.

Consequences on other surfaces MUST be honest: with retention enabled, SPEC-0016's "all-time" total becomes "total within the retention horizon", and both the stats page and the never-clicked dashboard panel MUST label their figures accordingly (e.g. "since {horizon date}" / "no clicks within retention") whenever retention is active; and time-series charts MUST render days older than the horizon as no-data rather than zero, per REQ "Per-Link Daily Time Series".

#### Scenario: Default is no deletion

- **WHEN** `JOE_CLICK_RETENTION` is unset and the server runs for months
- **THEN** no click row is ever deleted by the application

#### Scenario: Opt-in pruning

- **WHEN** `JOE_CLICK_RETENTION=365` and rows exist with `clicked_at` 400 days old and 300 days old
- **THEN** a pruner run deletes the 400-day-old rows, leaves the 300-day-old rows, logs the count, and increments `joelinks_clicks_pruned_total`

#### Scenario: Batched deletes

- **WHEN** 25,000 rows are older than the horizon at prune time
- **THEN** the pruner deletes them across at least three bounded batches within the run

#### Scenario: Labeled totals under retention

- **WHEN** retention is active and an owner views the stats page
- **THEN** the total is labeled as covering the retention window, not "all time"

#### Scenario: Startup surfacing

- **WHEN** the server starts with `JOE_CLICK_RETENTION=365`
- **THEN** a log line states that click retention is enabled with a 365-day horizon

---

### Requirement: Capability Gating of Analytics Surfaces

Every analytics surface MUST resolve authorization through the shared capability matrix (`internal/store/auth.go` `LinkCaps`, PR #255) or, for cross-link scope, through a store-layer method deriving the viewer's link set from the same primitives (`link_owners`, `link_shares`, admin role). Handlers MUST NOT reimplement ownership, share, or visibility logic. The complete gate assignment:

| Surface | Gate | Attribution (user identities) |
|---|---|---|
| Stats page incl. time series + breakdowns (web) | `CanStats` | Recent-clicks user column only when `CanManageShares` (PR #255 rule, stated normatively here — supersedes SPEC-0016's owner-only wording) |
| `GET /api/v1/links/{id}/stats`, `/clicks` (v1) | `CanStats` | `user` field only when `CanManageShares` (PR #255 rule, stated normatively here — supersedes SPEC-0016's owner-only wording) |
| `GET /api/v1/links/{id}/stats/timeseries` | `CanStats` | None present |
| `GET /api/v1/links/{id}/stats/breakdowns` | `CanStats` | None present |
| CSV export (both routes) | `CanStats` | `user` and raw `user_agent` columns populated only when `CanManageShares` |
| `/dashboard/analytics` + `GET /api/v1/analytics` | Authenticated; aggregates over the viewer's personal scope (own + co-owned + shared) | None present |
| `scope=all` on the dashboard/API | Admin role only | None present |

New analytics code MUST carry `// Governing: SPEC-0021 REQ …` comments at each gate site.

#### Scenario: One matrix, three surfaces agree

- **WHEN** a given user's `CanStats` is false for a link
- **THEN** the stats page returns 403, all four per-link API endpoints return 403, and the link contributes nothing to that user's dashboard

#### Scenario: Attribution never widens

- **WHEN** any surface introduced by this spec is rendered for a caller without `CanManageShares`
- **THEN** no user ID, display name, or other clicker identity appears anywhere in the response

---

## Security Requirements

### Authentication

All endpoints MUST require authentication by default. Public (unauthenticated) endpoints MUST be explicitly listed with justification.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `GET /dashboard/links/{id}/stats` (v2 additions) | Session; `CanStats` | — |
| `GET /dashboard/links/{id}/stats/export` | Session; `CanStats` | — |
| `GET /dashboard/analytics` | Session (any authenticated user; self-scoped) | — |
| `GET /api/v1/links/{id}/stats/timeseries` | Bearer PAT (SPEC-0006); `CanStats` | — |
| `GET /api/v1/links/{id}/stats/breakdowns` | Bearer PAT (SPEC-0006); `CanStats` | — |
| `GET /api/v1/links/{id}/stats/export` | Bearer PAT (SPEC-0006); `CanStats` | — |
| `GET /api/v1/analytics` | Bearer PAT (SPEC-0006); self-scoped, `scope=all` admin-only | — |

*(none public — no unauthenticated analytics surface exists)*

### CSV Injection

Exported CSV cells derived from attacker-influenceable columns (`referrer`, `user_agent`, `user` display names) MUST be neutralized against spreadsheet formula injection: any cell whose **first non-whitespace/control character** is `=`, `+`, `-`, or `@` — i.e. after skipping any run of leading whitespace or control characters (TAB 0x09, CR 0x0D, LF 0x0A, spaces) — MUST be prefixed with a single quote (`'`), and cells that begin with TAB (0x09) or CR (0x0D) MUST likewise be escaped (per OWASP guidance), all in addition to standard RFC 4180 quoting. Referrers are attacker-controlled by construction (any site can send any `Referer`), so this is mandatory, not defense-in-depth.

### Export Volume Caps

Exports MUST stream in bounded keyset batches (never buffering the full set), enforce the 100,000-row per-request cap, and remain gated by PAT/session auth — an export request cannot be amplified beyond one bounded table walk. If the REST API gains rate limiting, the export endpoints MUST adopt it first (they are the most expensive read in the system).

### Referrer PII

Referrer values are already sanitized at capture: `internal/handler/resolve.go` strips the query string and fragment from the `Referer` header before the click event is queued (`u.RawQuery = ""`, `u.Fragment = ""` — Governing comment cites SPEC-0016 REQ "Click Data Schema", "strip query/fragment to prevent token leakage"), and truncates to 2048. v2 read surfaces MUST NOT assume more than that: breakdowns further reduce referrers to **host only**, and the CSV export emits the stored (already query-stripped) value verbatim. Any pre-fix legacy rows containing query strings are still host-grouped on the breakdown surfaces; the export emits stored values as-is, which is the existing v1 `/clicks` exposure, not a widening.

### Clicker Attribution and the Share Roster

Per PR #255: the authenticated clicker set of a `secure` link approximates its hidden share roster, so identities are `CanManageShares`-only everywhere (see the gating table). Aggregate counts (time series, breakdowns, auth-vs-anon split, dashboard panels) are deliberately identity-free. The auth-vs-anon split is **roster-inert by construction**: a `secure` link — the only place a hidden share roster exists — can never record an anonymous click, because SPEC-0010's resolution order sends unauthenticated requests to login before any redirect and SPEC-0016 records clicks only on successful redirects; the split is degenerate (100% authenticated) exactly where roster inference would matter, so it adds zero bits about the roster beyond click counts recipients already see. The raw `user_agent` CSV column is `CanManageShares`-only (blank otherwise, same mechanism as `user`): per-row device fingerprints correlated with exact timestamps would let a recipient with out-of-band knowledge of roster members' devices probabilistically attribute clicks — adjacent to exactly what PR #255 sealed. Recipients keep the parsed `browser`/`os` family columns and the aggregate breakdowns, which carry no timestamp-correlated fingerprint.

### Retention Pruning vs Archived Links (SPEC-0020 Interplay)

The pruner deletes by click age uniformly; it MUST NOT consult link lifecycle state in v2. If SPEC-0020 (forthcoming, epic #217) requires archived links to retain historical stats past the retention horizon, that mechanism (e.g. summary counts frozen at archive time) belongs to SPEC-0020 and MUST NOT be implemented by weakening this spec's pruner semantics. Until SPEC-0020 lands, enabling retention means archived links lose aged click rows like all others — documented here so neither epic ships a silent assumption. The ≥ 90-day retention floor that protects SPEC-0020's `link_clicks`-computed staleness views is recorded normatively in REQ "Click Retention".

### UA Parsing DoS

The UA parser MUST be linear-time substring matching over input bounded to 512 runes, with no regular expressions and no backtracking (see REQ "User-Agent Parsing"). Hostile UA strings are stored today (capture accepts arbitrary values, truncated); the read-time parser is the component that must be robust to them, and its bound is structural, not a timeout.

### Cross-User Aggregation Leakage

The global dashboard and `/api/v1/analytics` MUST derive their link scope in the store layer from `link_owners` + `link_shares` (+ admin role for `scope=all`), and every panel query MUST be constrained to that ID set — there is no code path where an unconstrained aggregate is filtered afterward in the handler. Tests MUST assert that another user's public link never influences a viewer's panels (the strictest case, since `public` is the visibility most tempting to include).

---

## References

- ADR-0021 (Link Analytics v2 — implemented by this spec); epic issue #216
- SPEC-0002 (link data model — `links`/`link_owners`, `created_at`, and slug fields consumed by the dashboard scope derivation and panels)
- SPEC-0016 / ADR-0016 (link analytics v1 — required, not replaced: capture pipeline, `link_clicks` schema, v1 stats/clicks endpoints, and Prometheus baseline remain normative, modulo the two scoped amendments in the Overview: access-control supersession per PR #255, and retention; ADR-0016 pre-approved the latter)
- SPEC-0020 (link lifecycle, **forthcoming** — epic #217, drafted concurrently; owns archived-links stats preservation vs this spec's retention pruning)
- SPEC-0004 (application views/routing — dashboard routes, login redirects, HTMX fragment conventions)
- SPEC-0005 / ADR-0008 (REST API — error shape, response conventions the new endpoints follow)
- SPEC-0006 / ADR-0009 (PAT bearer auth — no-session rule on `/api/v1`, which forces the paired export routes)
- SPEC-0007 (OpenAPI/Swagger — annotation and `make swagger` obligations for all new endpoints)
- SPEC-0010 / ADR-0014 (visibility modes — `link_shares`, admin override style for `scope=all`; `public` ≠ stats access)
- SPEC-0012 (public browsing — the baseline showing why public links are browsable but not stats-readable)
- SPEC-0018 / ADR-0018 (MCP — closed v1 tool inventory; v2 analytics parity for `get_link_stats` deferred to a SPEC-0018 revision)
- PR #255 (capability matrix `internal/store/auth.go`, manager-only clicker attribution — the authorization baseline)
- PR #242 (keyset `(clicked_at, id)` pagination — reused by the export iterator and cursor; index follow-up note applies)
- PR #257 / issue #206 (interim UTC-label timestamp rendering — the landed state superseded by REQ "Viewer-Local Timestamps")
- `internal/handler/resolve.go` (referrer query/fragment stripping at capture — cited under Referrer PII)
