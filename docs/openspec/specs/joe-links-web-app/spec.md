# SPEC-0001: joe-links Web Application

## Overview

**Joe Links** is a self-hosted "go links" service — a riff on [Go Links](https://www.golinks.io/). It lets authenticated users create short, memorable slugs (e.g., `jira`, `standup`, `onboarding`) that redirect to long URLs. Visiting `https://joe-links.example.com/jira` instantly redirects the browser to whatever URL `jira` maps to. Users manage their own links; admins can manage any link.

The application is a Go-based server-rendered web app using HTMX for hypermedia interactions, DaisyUI/Tailwind for UI, a pluggable relational database backend with versioned migrations, and authentication that is OIDC where a provider is configured and a single local admin account where none is, with local user management and RBAC.

All environment variables are prefixed `JOE_`.

See ADR-0001 (Technology Stack), ADR-0002 (Database), ADR-0003 (AuthN/AuthZ), ADR-0024 (optional OIDC, local admin account), ADR-0004 (CLI Framework).

---

## Requirements

### Requirement: CLI Entrypoint

The application MUST be structured as a Cobra CLI with Viper configuration. The root command MUST be `joe-links`. Viper MUST use `JOE_` as its environment variable prefix so that all config is read from `JOE_`-namespaced env vars. At minimum two subcommands MUST be provided:

- `joe-links serve` — runs pending migrations then starts the HTTP server
- `joe-links migrate` — runs pending migrations and exits (for init-container use)

An optional config file (`joe-links.yaml`) SHOULD be supported for local development.

#### Scenario: Serve Command Startup

- **WHEN** `joe-links serve` is executed with valid configuration
- **THEN** pending migrations MUST be applied and the HTTP server MUST begin accepting requests

#### Scenario: Migrate Command

- **WHEN** `joe-links migrate` is executed
- **THEN** all pending migrations MUST be applied and the process MUST exit with code 0 on success

#### Scenario: Missing Required Configuration

- **WHEN** a required environment variable (e.g., `JOE_DB_DSN`) is absent at startup
- **THEN** the command MUST log a descriptive error and exit with a non-zero status code

---

### Requirement: Go HTTP Server

The HTTP server MUST be implemented in Go using a `net/http`-compatible router. The bind address MUST be configurable via `JOE_HTTP_ADDR` (default `:8080`). The compiled binary MUST embed all static assets, templates, and migration files so that no external files are required at runtime.

#### Scenario: Default Bind Address

- **WHEN** `JOE_HTTP_ADDR` is not set
- **THEN** the server MUST listen on `:8080`

#### Scenario: Custom Bind Address

- **WHEN** `JOE_HTTP_ADDR=0.0.0.0:9000` is set
- **THEN** the server MUST listen on `0.0.0.0:9000`

---

### Requirement: Short Link Resolution

This is the core feature. The application MUST resolve short link slugs by redirecting the browser to the target URL. A request to `/{slug}` MUST look up the slug in the database and issue a `302 Found` redirect to the stored URL. The following slugs are reserved (exact match — dash-prefixed slugs like `links-roundup` remain valid) and MUST NOT be valid slugs: `admin`, `api`, `auth`, `dashboard`, `links`, `mcp`, `metrics`, `static`, `u`. The single source of truth for this list is `store.ReservedSlugs()` in `internal/store/validate.go`. If a slug is not found, the application MUST return a friendly 404 page. `HEAD` requests MUST behave identically to `GET` for resolution (same status code and `Location` header) with no response body and no click recording (SPEC-0016).

#### Scenario: Valid Slug

- **WHEN** a request arrives at `/{slug}`, the slug exists in the database, and the link's URL contains no variable placeholders (SPEC-0009)
- **THEN** the server MUST respond with `302 Found` and a `Location` header pointing to the stored URL. A bare-slug visit to a variable link is an arity mismatch and MUST 404 instead (ADR-0013, SPEC-0009)

#### Scenario: Unknown Slug

- **WHEN** a request arrives at `/{slug}` and no matching slug exists
- **THEN** the server MUST respond with a 404 page that includes a prompt to create the link

#### Scenario: Reserved Path

- **WHEN** a request arrives at a reserved prefix path (e.g., `/auth/login`, `/static/css/app.css`)
- **THEN** the reserved route handler MUST take precedence and the slug resolver MUST NOT be invoked

---

### Requirement: Short Link Management

Authenticated users MUST be able to create, view, edit, and delete their own short links via a dashboard UI. Each link MUST have: `id`, `slug` (unique, URL-safe), `url` (the redirect target), `owner_id` (FK to `users.id`), `description` (optional), `created_at`, `updated_at`. The `slug` MUST match the pattern `[a-z0-9][a-z0-9\-]*[a-z0-9]` (lowercase alphanumeric and hyphens, not starting or ending with a hyphen; single-character slugs of `[a-z0-9]` are also valid). Slugs MUST be globally unique. A user MUST NOT be able to edit or delete links owned by another user unless they have the `admin` role.

#### Scenario: Create Link

- **WHEN** an authenticated user submits a valid slug and URL
- **THEN** a new link record MUST be created with the submitting user as owner and the browser MUST be redirected to the dashboard

#### Scenario: Duplicate Slug

- **WHEN** an authenticated user submits a slug that already exists
- **THEN** the form MUST return a validation error indicating the slug is taken

#### Scenario: Invalid Slug Format

- **WHEN** an authenticated user submits a slug containing uppercase letters, spaces, or invalid characters
- **THEN** the form MUST return a validation error describing the allowed format

#### Scenario: Edit Own Link

- **WHEN** an authenticated user submits an edit for a link they own
- **THEN** the link MUST be updated with the new values

#### Scenario: Edit Another User's Link (Non-Admin)

- **WHEN** an authenticated user with role `user` attempts to edit a link they do not own
- **THEN** the server MUST return `403 Forbidden`

#### Scenario: Admin Edits Any Link

- **WHEN** an authenticated user with role `admin` edits any link regardless of ownership
- **THEN** the edit MUST succeed

#### Scenario: Delete Link

- **WHEN** an authenticated owner (or admin) confirms deletion of a link
- **THEN** the link record MUST be permanently deleted

---

### Requirement: HTMX Hypermedia Interactions

The application MUST use HTMX to drive dynamic UI interactions via server-rendered HTML fragments. Client-side JavaScript beyond HTMX SHOULD be minimized. The server MUST respond to HTMX partial requests with HTML fragments rather than full page renders when the `HX-Request` header is present. Full JSON API endpoints for UI purposes MUST NOT be created.

#### Scenario: HTMX Partial Request

- **WHEN** a browser sends a request with the `HX-Request: true` header
- **THEN** the handler MUST return an HTML fragment suitable for HTMX target swapping, not a full page layout

#### Scenario: Non-HTMX Request

- **WHEN** a browser sends a request without the `HX-Request` header
- **THEN** the handler MUST return a full HTML page including the base layout

---

### Requirement: DaisyUI and Tailwind CSS

The application UI MUST use Tailwind CSS for utility-class styling and DaisyUI as the component layer. A Tailwind build step MUST produce a compiled CSS file served as a static asset and embedded in the Go binary. Custom CSS beyond Tailwind utilities and DaisyUI component overrides SHOULD be avoided.

#### Scenario: CSS Asset Serving

- **WHEN** a browser requests `/static/css/app.css`
- **THEN** the server MUST respond with the compiled Tailwind/DaisyUI CSS with `Content-Type: text/css`

#### Scenario: Theming

- **WHEN** a DaisyUI theme is configured in `static/css/input.css`
- **THEN** all rendered pages MUST apply that theme consistently

---

### Requirement: CSS Freshness in CI

The committed compiled stylesheet (`web/static/css/app.css`) MUST match what the pinned Tailwind/DaisyUI toolchain produces from the templates and configuration. The project's CI MUST include a step that rebuilds the CSS and verifies the output matches the committed file, failing with a message indicating the CSS needs to be regenerated. This mirrors SPEC-0007 REQ "Spec Freshness in CI" for the swagger artifacts.

#### Scenario: Stale Committed CSS Fails CI

- **WHEN** a template or Tailwind configuration change alters the compiled CSS but the committed `web/static/css/app.css` is not regenerated
- **THEN** the CI check MUST fail, indicating the CSS must be rebuilt and committed

---

### Requirement: Pluggable Database Backend

The application MUST support SQLite, MariaDB, and PostgreSQL. The active backend MUST be selected at runtime via `JOE_DB_DRIVER` (values: `sqlite3`, `mysql`, `postgres`). The connection string MUST be provided via `JOE_DB_DSN`. The application MUST NOT hardcode database-specific SQL outside of migration files.

#### Scenario: SQLite Backend

- **WHEN** `JOE_DB_DRIVER=sqlite3` and `JOE_DB_DSN` points to a valid file path or `:memory:`
- **THEN** the application MUST start and all database operations MUST succeed

#### Scenario: PostgreSQL Backend

- **WHEN** `JOE_DB_DRIVER=postgres` and `JOE_DB_DSN` contains a valid PostgreSQL DSN
- **THEN** the application MUST start and all database operations MUST succeed

#### Scenario: Unsupported Driver

- **WHEN** `JOE_DB_DRIVER` is set to an unrecognized value
- **THEN** the application MUST exit with a descriptive error

---

### Requirement: Database Schema Migrations

The application MUST use `goose` for versioned schema migrations embedded via `//go:embed`. Migrations MUST be applied automatically by `joe-links serve` before the HTTP server starts. Migrations MUST be idempotent.

#### Scenario: First-Time Startup

- **WHEN** the application starts against a fresh database
- **THEN** all pending migrations MUST be applied in order before the server accepts requests

#### Scenario: Already-Migrated Database

- **WHEN** all migrations have already been applied
- **THEN** the migration step MUST complete without error or data modification

#### Scenario: Migration Failure

- **WHEN** a migration fails to apply
- **THEN** the application MUST log the error and exit without starting the HTTP server

---

### Requirement: Authentication Mode Selection

The application MUST select its login path from configuration at startup. If any of `JOE_OIDC_ISSUER`, `JOE_OIDC_CLIENT_ID`, `JOE_OIDC_CLIENT_SECRET`, or `JOE_OIDC_REDIRECT_URL` is set, it MUST run in OIDC mode and require the remaining three; OIDC claims MUST be trusted as authoritative, and password authentication MUST NOT be reachable. If none is set, it MUST run in local mode as defined by "Local Admin Authentication". Setting both a provider and `JOE_ADMIN_PASSWORD` MUST fail startup. A deployment that configures a provider MUST behave exactly as it did before local mode existed.

Governing: ADR-0003, ADR-0024.

#### Scenario: OIDC Configured — Discovery at Startup

- **WHEN** the four `JOE_OIDC_*` settings are present
- **THEN** the application MUST perform OIDC discovery before serving, and MUST exit without starting the HTTP server if discovery fails

#### Scenario: No Provider Configured

- **WHEN** no `JOE_OIDC_*` setting is present and `JOE_ADMIN_PASSWORD` is set
- **THEN** the application MUST start in local mode and MUST NOT attempt OIDC discovery

#### Scenario: No Login Path Configured

- **WHEN** no `JOE_OIDC_*` setting is present and `JOE_ADMIN_PASSWORD` is unset
- **THEN** the application MUST exit with an error naming both configuration paths, and MUST NOT start the HTTP server

#### Scenario: Both Paths Configured

- **WHEN** a `JOE_OIDC_*` setting and `JOE_ADMIN_PASSWORD` are both present
- **THEN** the application MUST exit with an error naming the conflict, because a password beside a working provider is a second, weaker door

#### Scenario: Initiating Login

- **WHEN** an unauthenticated user navigates to a protected route in OIDC mode
- **THEN** the application MUST redirect to the OIDC provider's authorization endpoint with a `state` parameter and PKCE `code_challenge`

#### Scenario: Successful OIDC Callback

- **WHEN** the OIDC provider redirects to `/auth/callback` with a valid code and matching state
- **THEN** the application MUST exchange the code, verify the ID token, upsert the user record, create a session, and redirect to the originally requested URL or `/dashboard`

#### Scenario: Invalid State Parameter

- **WHEN** the callback arrives with a mismatched `state`
- **THEN** the application MUST return `400 Bad Request` and MUST NOT create a session

#### Scenario: Token Verification Failure

- **WHEN** the ID token fails signature verification or has invalid claims
- **THEN** the application MUST return `401 Unauthorized`

#### Scenario: Password Sign-In Refused in OIDC Mode

- **WHEN** `POST /auth/login` arrives with credentials in OIDC mode
- **THEN** the application MUST return `405 Method Not Allowed` and MUST NOT create a session

---

### Requirement: Local Admin Authentication

In local mode the application MUST sign in exactly one local administrator: the account named by `JOE_ADMIN_USERNAME` (default `admin`), authenticated against `JOE_ADMIN_PASSWORD`. The password MUST be at least 12 characters, MUST NOT be stored in plaintext, and MUST be kept as a salted PBKDF2-HMAC-SHA256 hash with self-describing parameters in `local_credentials`, keyed to the `users` row; `users` MUST NOT gain a password column. The account MUST be upserted on every startup as `users(provider='local', subject=<username>)` with role `admin` granted (grant-only, never revoked by startup), so rotating `JOE_ADMIN_PASSWORD` takes effect on the next restart and no stale hash outlives the configuration that set it. The application MUST NOT offer signup, self-registration, or password change in either mode: an instance with no provider serves this one account and no other.

Failed sign-ins MUST be counted per client address as resolved by ADR-0023 (never from an untrusted header), and past 10 failures within 5 minutes the application MUST return `429` with a `Retry-After` header. Every failed attempt MUST report one indistinguishable error, whether the username, the password, or both were wrong. `return_url` on this path MUST be sanitized by the same rule the OIDC callback uses.

Governing: ADR-0024, ADR-0002, ADR-0023.

#### Scenario: First Boot in Local Mode

- **WHEN** the server starts in local mode with no `users` row for the configured username
- **THEN** the application MUST create it with role `admin`, store a freshly salted hash of `JOE_ADMIN_PASSWORD`, and serve the sign-in form at `GET /auth/login`

#### Scenario: Successful Local Sign-In

- **WHEN** `POST /auth/login` presents the configured username and a password matching the stored hash
- **THEN** the application MUST renew the session token, store `user_id` and `role`, and redirect to the sanitized `return_url` or `/dashboard`

#### Scenario: Failed Local Sign-In

- **WHEN** `POST /auth/login` presents any other username or password
- **THEN** the application MUST return `401 Unauthorized` with the same message in every case, MUST NOT create a session, and MUST NOT reveal which half was wrong

#### Scenario: Credential Guessing Is Throttled

- **WHEN** an address has accumulated 10 failed local sign-ins within 5 minutes
- **THEN** the application MUST return `429` with `Retry-After`, including for a request presenting the correct password

#### Scenario: Rotated Password Takes Effect

- **WHEN** the server restarts with a different `JOE_ADMIN_PASSWORD`
- **THEN** the stored hash MUST be replaced, and the previous password MUST no longer sign in

---

### Requirement: Local User Records

The application MUST maintain a `users` table with at minimum: `id`, `provider`, `subject`, `email`, `display_name`, `role`, `created_at`, `updated_at`. Records are keyed on `(provider, subject)`. On authentication, the record MUST be upserted. Env-driven admin configuration is grant-only and applies on EVERY login, not just the first: if the authenticated email matches `JOE_ADMIN_EMAIL`, or the token's groups claim (name per `JOE_OIDC_GROUPS_CLAIM`, default `groups`) intersects `JOE_OIDC_ADMIN_GROUPS`, the user MUST hold role `admin` after that login — including a user previously demoted via the admin UI. Env grants never demote: a user matching no grant keeps their stored `role`. Explicit demotion is possible only through the admin UI/API, and lasts only until the next login for env-matching users. (Grant-only-on-any-login semantics per PR #239; see also `joe-links-web-app/design.md`.)

#### Scenario: New User — Default Role

- **WHEN** a user authenticates for the first time and their email does not match `JOE_ADMIN_EMAIL`
- **THEN** a new record MUST be created with role `user`

#### Scenario: New User — Admin Email Match

- **WHEN** a user authenticates for the first time and their email matches `JOE_ADMIN_EMAIL`
- **THEN** a new record MUST be created with role `admin`

#### Scenario: Returning User

- **WHEN** a matching `(provider, subject)` record exists and the user matches no env grant
- **THEN** `email` and `display_name` MUST be updated from OIDC claims; the stored `role` MUST be preserved

#### Scenario: Env-Granted User Re-Promoted After UI Demotion

- **WHEN** a user whose email matches `JOE_ADMIN_EMAIL` was demoted to `user` via the admin UI and then logs in again
- **THEN** the record MUST hold role `admin` after login (env grants apply on every login and never demote)

---

### Requirement: Server-Side Sessions

The application MUST use `alexedwards/scs` with a database-backed session store. Sessions MUST have a 30-day absolute expiry with no idle timeout. The expiry MUST be configurable via `JOE_SESSION_LIFETIME` (default `720h`). Session cookies MUST be `HttpOnly` and `Secure` in production.

#### Scenario: Authenticated Request

- **WHEN** a browser sends a valid session cookie
- **THEN** the request context MUST contain the authenticated user's `id` and `role`

#### Scenario: Expired Session

- **WHEN** the session has exceeded its 30-day absolute expiry
- **THEN** the application MUST treat the request as unauthenticated and redirect to login

#### Scenario: Session Logout

- **WHEN** an authenticated user sends `POST /auth/logout`
- **THEN** the server MUST destroy the session record and clear the cookie

---

### Requirement: Role-Based Access Control

Two roles MUST be defined: `user` and `admin`. Route-level authorization MUST be enforced via HTTP middleware. `admin`-only routes MUST return `403 Forbidden` for `user`-role requests.

#### Scenario: Admin Route Access by Admin

- **WHEN** an `admin` user accesses an admin-only route
- **THEN** the request MUST proceed to the handler

#### Scenario: Admin Route Access by Non-Admin

- **WHEN** a `user`-role user accesses an admin-only route
- **THEN** the middleware MUST return `403 Forbidden`

#### Scenario: Unauthenticated Access to Protected Route

- **WHEN** an unauthenticated user accesses any protected route
- **THEN** the middleware MUST redirect to `/auth/login` with the original URL as a `redirect` query parameter
