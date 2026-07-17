# SPEC-0016: Link Analytics and Prometheus Metrics

## Overview

joe-links currently records no data about link usage. This spec formalises the
requirements for click tracking, per-link analytics pages, a Prometheus metrics
endpoint, and REST API extensions for programmatic access to click data.
See ADR-0016 for the architectural decisions that govern this capability.

## Requirements

### Requirement: Click Recording

The resolver MUST record a click event for every successful `GET` redirect it
issues. `HEAD` requests MUST be excluded from click recording (precedent: the
keyword-redirect exclusion below) — Prometheus redirect counters still include
them. Resolutions blocked by lifecycle state (expired or archived links,
SPEC-0020) issue no redirect and MUST record no click.
A click event SHALL be inserted asynchronously via a buffered channel so that
the 302 redirect response is not blocked on the database write. The channel
buffer MUST be at least 256 events. Events that cannot be queued because the
channel is full MAY be dropped; the resolver MUST NOT block waiting for the
channel. The click writer goroutine MUST drain the channel on shutdown.

#### Scenario: Successful redirect recorded

- **WHEN** a request resolves a known slug and a 302 redirect is issued
- **THEN** a click event is sent to the buffered channel within the same request handler

#### Scenario: Full channel — drop, do not block

- **WHEN** the buffered click channel is full at the time of redirect
- **THEN** the click event is dropped and the 302 response is issued without delay

#### Scenario: Graceful shutdown drain

- **WHEN** the server receives a shutdown signal
- **THEN** the click writer goroutine finishes processing all queued events before the process exits

#### Scenario: Keyword redirects not recorded

- **WHEN** a request matches a keyword host or keyword path prefix and is redirected
- **THEN** no click event is recorded (keyword redirects are not link-owned slugs)

#### Scenario: HEAD redirects not recorded

- **WHEN** a `HEAD` request resolves a known slug and receives the same 302 a `GET` would
- **THEN** no click event is recorded; `joelinks_redirects_total{status="found"}` still increments

---

### Requirement: Click Data Schema

The `link_clicks` table MUST be created via a goose migration. Each row SHALL
represent a single click event and MUST contain:

| Column       | Type     | Constraints                                    |
|--------------|----------|------------------------------------------------|
| `id`         | TEXT/UUID | Primary key                                   |
| `link_id`    | TEXT/UUID | FK → `links.id` ON DELETE CASCADE             |
| `user_id`    | TEXT/UUID | FK → `users.id` ON DELETE SET NULL; nullable  |
| `ip_hash`    | TEXT     | SHA-256(client IP + daily salt); NOT NULL      |
| `user_agent` | TEXT     | App-truncated to 512 chars; nullable           |
| `referrer`   | TEXT     | App-truncated to 2048 chars; nullable          |
| `clicked_at` | DATETIME | UTC timestamp; NOT NULL                        |

The `user_agent` (512) and `referrer` (2048) length limits MUST be enforced in
the application layer at insert time, not as a database column constraint: the
columns are declared `TEXT` so the schema stays portable across SQLite, MySQL,
and PostgreSQL. Truncation MUST be rune-aware (count Unicode code points, never
split a multi-byte character).

A composite index on `(link_id, clicked_at DESC)` MUST be created to support
per-link recent-click queries without full table scans. Known follow-up (PR
#242): the keyset pagination in REQ "REST API Clicks Endpoint" orders by
`clicked_at DESC, id DESC`, which this index no longer fully covers — a future
migration SHOULD add `id` to the index to restore index-ordered pagination for
high-volume links on MySQL/SQLite. The `user_id` column
MUST be set to the authenticated user's ID when the request carries a session,
and NULL otherwise.

The `ip_hash` MUST be computed as `SHA-256(clientIP + ":" + dailySalt)` where
`dailySalt` is derived from the current UTC date (format `YYYYMMDD`). The salt
MUST rotate at UTC midnight.

#### Scenario: Authenticated click

- **WHEN** a logged-in user clicks a link
- **THEN** the `user_id` column is set to that user's ID

#### Scenario: Anonymous click

- **WHEN** an unauthenticated request triggers a redirect
- **THEN** the `user_id` column is NULL and only `ip_hash` identifies the source

#### Scenario: IP hash rotation

- **WHEN** two clicks occur from the same IP address on different UTC dates
- **THEN** the `ip_hash` values are different (daily salt prevents cross-day correlation)

---

### Requirement: Prometheus Metrics Endpoint

The application MUST expose a Prometheus-compatible metrics endpoint at
`GET /metrics` using `prometheus/client_golang` and `promhttp.Handler()`.
The endpoint MUST NOT require authentication. The following metrics MUST be
registered and updated:

| Metric name                             | Type      | Labels            | Description                               |
|-----------------------------------------|-----------|-------------------|-------------------------------------------|
| `joelinks_redirects_total`              | Counter   | `status`          | Total slug resolutions (`found`, `not_found`) |
| `joelinks_redirect_duration_seconds`    | Histogram | —                 | Time from request receipt to redirect response |
| `joelinks_clicks_recorded_total`        | Counter   | —                 | Click rows successfully written to DB     |
| `joelinks_clicks_record_errors_total`   | Counter   | —                 | Click insert failures                     |
| `joelinks_links_total`                  | Gauge     | —                 | Total links currently in the database     |
| `joelinks_users_total`                  | Gauge     | —                 | Total users currently in the database     |

The `joelinks_links_total` and `joelinks_users_total` gauges SHOULD be updated
on a background interval (e.g., every 60 seconds) rather than on every request.
A `slug` label MUST NOT be added to any counter or histogram (cardinality concern).

#### Scenario: Prometheus scrape

- **WHEN** Prometheus sends `GET /metrics`
- **THEN** the response is HTTP 200 with `Content-Type: text/plain; version=0.0.4` and all registered metrics in the Prometheus text exposition format

#### Scenario: Redirect counter increments

- **WHEN** a slug resolves successfully and a 302 is issued
- **THEN** `joelinks_redirects_total{status="found"}` increments by 1

#### Scenario: Not-found counter increments

- **WHEN** the resolver renders a 404 — because the slug lookup returned no result, or because the lookup found a variable link visited bare-slug (arity mismatch, SPEC-0009)
- **THEN** `joelinks_redirects_total{status="not_found"}` increments by 1

#### Scenario: Histogram records latency

- **WHEN** a redirect request completes
- **THEN** `joelinks_redirect_duration_seconds` records the elapsed time in seconds

---

### Requirement: Link Stats Dashboard Page

A per-link analytics page MUST be available at
`GET /dashboard/links/{id}/stats` within the authenticated dashboard. Access is
governed by the `LinkCaps` capability matrix (`CanStats`): the link's owner(s),
co-owners, admin users, **and share recipients** (users with a `link_shares`
record, SPEC-0010) MAY access this page — SPEC-0021 supersedes this spec's
original owner-only wording. Clicker attribution (the display name of the
clicking user) MUST be shown only to viewers with `CanManageShares` (owners,
co-owners, admins) — share recipients MUST NOT see it (SPEC-0021). All other
authenticated users MUST receive a 403 response; unauthenticated requests MUST
be redirected to the login page.

The page MUST display:

- Total click count (all time)
- Click count for the last 7 days
- Click count for the last 30 days
- A table of the most recent 50 clicks, showing: timestamp, referrer (or "—" if
  absent), and the user's display name if `user_id` is non-null (otherwise "anonymous")

The page MUST follow existing HTMX/DaisyUI conventions: full page render on
direct navigation, fragment render on HTMX request. Timestamps MUST be rendered
in UTC with RFC 3339 `title` attributes on the rendered elements (interim
behavior until SPEC-0021's viewer-local timezone rendering lands).

#### Scenario: Owner views stats

- **WHEN** an authenticated link owner navigates to `/dashboard/links/{id}/stats`
- **THEN** the stats page renders with totals and recent clicks

#### Scenario: Non-owner denied

- **WHEN** an authenticated user who is not an owner, co-owner, admin, or share recipient navigates to `/dashboard/links/{id}/stats`
- **THEN** a 403 Forbidden response is returned

#### Scenario: Unauthenticated redirect

- **WHEN** an unauthenticated request reaches `/dashboard/links/{id}/stats`
- **THEN** the user is redirected to `/auth/login` with a `redirect` query parameter

#### Scenario: Link with no clicks

- **WHEN** the link has never been clicked
- **THEN** all counters display 0 and the recent-clicks table shows an empty state message

---

### Requirement: REST API Stats Endpoint

`GET /api/v1/links/{id}/stats` MUST return a JSON summary of click counts for the
specified link. The endpoint MUST require bearer token authentication (following
ADR-0009 conventions). Authorization follows `CanStats`: link owners, co-owners,
admins, and share recipients (SPEC-0010) MUST be authorised — SPEC-0021 supersedes
the original owner-only wording; other authenticated callers MUST receive 403. A
non-existent link MUST return 404.

The response body MUST conform to:

```json
{
  "link_id": "uuid",
  "total":   1024,
  "last_7d": 42,
  "last_30d": 310
}
```

#### Scenario: Owner fetches stats via API

- **WHEN** an authenticated link owner calls `GET /api/v1/links/{id}/stats`
- **THEN** a 200 response with the JSON stats summary is returned

#### Scenario: Unknown link

- **WHEN** the `{id}` does not correspond to an existing link
- **THEN** a 404 JSON error response is returned

#### Scenario: Unauthorized caller

- **WHEN** a valid bearer token is presented but the token owner holds no `CanStats` capability on the link (not an owner, co-owner, admin, or share recipient)
- **THEN** a 403 JSON error response is returned

---

### Requirement: REST API Clicks Endpoint

`GET /api/v1/links/{id}/clicks` MUST return a paginated list of click events for
the specified link. The endpoint MUST support cursor-based pagination via an
optional `before` query parameter and a `limit` parameter (default 50, maximum
200). `before` MUST accept either an opaque `next_cursor` value from a previous
response or, for back-compat, a legacy bare RFC 3339 `clicked_at` timestamp;
any other value MUST be rejected with 400. The response MUST include a
`next_cursor` field set to an opaque SPEC-0005-style keyset cursor encoding
`(clicked_at, id)` of the last item when more results exist, and `null` when
the page is the last page. Authorization rules are identical to the stats
endpoint (`CanStats` — share recipients included; clicker attribution in the
`user` field is `CanManageShares`-only per SPEC-0021).

Each item in the `clicks` array MUST include:

```json
{
  "clicked_at": "2026-02-27T12:00:00Z",
  "referrer":   "https://example.com",
  "user":       { "id": "uuid", "display_name": "Alice" }
}
```

The `user` field MUST be `null` for anonymous clicks. The `referrer` field MUST
be `null` when absent.

#### Scenario: Paginated fetch

- **WHEN** a caller requests clicks with `limit=10`
- **THEN** at most 10 click objects are returned and `next_cursor` is set if more exist

#### Scenario: Cursor pagination

- **WHEN** a caller passes `before=<cursor>` from a previous response
- **THEN** only clicks strictly before that `(clicked_at, id)` keyset position are returned (a legacy timestamp cursor degrades to strict `clicked_at <`)

#### Scenario: Invalid cursor rejected

- **WHEN** a caller passes a `before` value that is neither an opaque cursor nor an RFC 3339 timestamp
- **THEN** the server responds 400 with a JSON error body

#### Scenario: Anonymous click in list

- **WHEN** a click row has a NULL `user_id`
- **THEN** the `user` field in the API response is `null`
