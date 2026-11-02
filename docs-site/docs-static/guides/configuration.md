---
title: "Configuration"
sidebar_label: "Configuration"
sidebar_position: 3
---

# Configuration

joe-links is configured via environment variables (prefixed with `JOE_`) or a `joe-links.yaml` config file. Environment variables take precedence over the config file.

## Environment Variables

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `JOE_HTTP_ADDR` | `:8080` | No | HTTP listen address (host:port) |
| `JOE_DB_DRIVER` | -- | Yes | Database driver: `sqlite3`, `mysql`, or `postgres` |
| `JOE_DB_DSN` | -- | Yes | Database connection string (see examples below) |
| `JOE_OIDC_ISSUER` | -- | OIDC mode | OIDC provider discovery URL (must serve `/.well-known/openid-configuration`) |
| `JOE_OIDC_CLIENT_ID` | -- | OIDC mode | OAuth2 client ID from your OIDC provider |
| `JOE_OIDC_CLIENT_SECRET` | -- | OIDC mode | OAuth2 client secret from your OIDC provider |
| `JOE_OIDC_REDIRECT_URL` | -- | OIDC mode | Full callback URL (e.g., `https://go.example.com/auth/callback`) |
| `JOE_ADMIN_USERNAME` | `admin` | No | Local mode: the username of the single local admin account (used only when no OIDC setting is set) |
| `JOE_ADMIN_PASSWORD` | -- | Local mode | Local mode: password for that account. At least 12 characters; stored only as a salted PBKDF2-HMAC-SHA256 hash. Refused together with any `JOE_OIDC_*` setting |
| `JOE_ADMIN_EMAIL` | -- | No | Email address permanently granted the `admin` role on every login (OIDC mode; in local mode it defaults to `<username>@local`) |
| `JOE_OIDC_ADMIN_GROUPS` | -- | No | Comma-separated OIDC group names whose members are granted the `admin` role (see below) |
| `JOE_OIDC_GROUPS_CLAIM` | `groups` | No | OIDC token claim that contains the user's group list |
| `JOE_SHORT_KEYWORD` | *(first DNS label of server hostname)* | No | Short-link prefix used in the UI and browser extension. Derived from the server hostname at request time — `go` from `go.example.com`, `links` from `links.example.com`, `localhost` from `localhost:8080`. Set explicitly if your hostname doesn't match your desired keyword |
| `JOE_SESSION_LIFETIME` | `720h` | No | Session absolute expiry as a Go duration string |
| `JOE_INSECURE_COOKIES` | `false` | No | Set to `true` to disable the `Secure` cookie flag (for local HTTP development) |
| `JOE_LLM_PROVIDER` | -- | No | Enables AI metadata suggestions. One of `anthropic`, `openai`, or `openai-compatible`. Unset = feature disabled |
| `JOE_LLM_API_KEY` | -- | No | API key for the LLM provider. Optional for keyless `openai-compatible` endpoints (e.g. Ollama) |
| `JOE_LLM_MODEL` | *(provider default)* | No | Model name. Defaults: `claude-haiku-4-5-20251001` (anthropic), `gpt-4o-mini` (openai/compatible) |
| `JOE_LLM_BASE_URL` | -- | No | Base URL override for `openai-compatible` providers (e.g. `http://localhost:11434/v1` for Ollama) |
| `JOE_LLM_PROMPT` | *(built-in template)* | No | Override the built-in suggestion prompt template |

## AI Metadata Suggestions

Set `JOE_LLM_PROVIDER` to enable the optional AI suggestion endpoint (`POST /api/v1/links/suggest`), which the browser extension uses to pre-fill a link's slug, title, description, and tags from its URL. See the [API Guide](./api-guide.md#suggest-link-metadata) for the request and response shape. When `JOE_LLM_PROVIDER` is unset the feature is completely disabled — no LLM calls are made and the endpoint returns `503`.

| Provider | `JOE_LLM_PROVIDER` | Notes |
|----------|--------------------|-------|
| Anthropic | `anthropic` | Requires `JOE_LLM_API_KEY`. Default model `claude-haiku-4-5-20251001` |
| OpenAI | `openai` | Requires `JOE_LLM_API_KEY`. Default model `gpt-4o-mini` |
| OpenAI-compatible | `openai-compatible` | Set `JOE_LLM_BASE_URL` (e.g. Ollama); `JOE_LLM_API_KEY` optional |

```
# Example: local Ollama, no API key required
JOE_LLM_PROVIDER=openai-compatible
JOE_LLM_BASE_URL=http://localhost:11434/v1
JOE_LLM_MODEL=llama3.2
```

## Authentication Mode

joe-links picks its login path from what you configure, and refuses to start if
the choice is ambiguous.

| Configuration | Mode | Sign-in |
|---------------|------|---------|
| Any `JOE_OIDC_*` setting (all four required) | OIDC | Redirect to your provider; no password is ever checked |
| No `JOE_OIDC_*` setting, `JOE_ADMIN_PASSWORD` set | Local | `GET /auth/login` form, one admin account (`JOE_ADMIN_USERNAME`, default `admin`) |
| Neither | -- | Startup error: joe-links never serves without a login path |
| Both | -- | Startup error: a password beside a working provider is a second, weaker door |

Local mode exists for home instances, for trying joe-links out, and for an
instance an auth proxy already fronts. It stores no plaintext password: the
account's hash (salted PBKDF2-HMAC-SHA256) lives in `local_credentials`, separate
from `users`, and is rewritten on every startup from `JOE_ADMIN_PASSWORD`, so
rotating the env var rotates the password. There is no signup, no password-reset
flow and no second local account — if you need per-person identities, configure a
provider. Failed sign-ins are throttled per client address: 10 attempts per 5
minutes, then `429` with `Retry-After`.

## Admin Role Assignment

There are two ways to grant a user the `admin` role. Both are evaluated on every login — if either condition matches, the user is promoted to `admin`.

### By email address

```
JOE_ADMIN_EMAIL=you@example.com
```

Simple and sufficient for a single administrator. Whoever logs in with this email address always gets the `admin` role, regardless of what the admin UI shows.

### By OIDC group membership

```
JOE_OIDC_ADMIN_GROUPS=admins,homelab-owners
JOE_OIDC_GROUPS_CLAIM=groups
```

Any user whose OIDC token contains one of the listed group names is granted `admin` on login. Useful when your OIDC provider (Authentik, Keycloak, Dex, etc.) already manages group membership.

By default joe-links looks for groups in a claim named `groups`. If your provider uses a different claim name (e.g. `roles` or a namespaced claim), set `JOE_OIDC_GROUPS_CLAIM` to match.

:::note
The admin UI role toggle writes directly to the database, but the role is re-evaluated from your OIDC config on every login. If you want a role change to stick permanently, manage it through `JOE_ADMIN_EMAIL` or `JOE_OIDC_ADMIN_GROUPS` rather than the UI.
:::

## Config File

You can also use a YAML config file at `joe-links.yaml` in the working directory:

```yaml
http:
  addr: ":8080"

db:
  driver: sqlite3
  dsn: ./joe-links.db

oidc:
  issuer: https://accounts.google.com
  client_id: your-client-id
  client_secret: your-client-secret
  redirect_url: https://go.example.com/auth/callback
  admin_groups: "admins,homelab-owners"
  groups_claim: groups

admin_email: admin@example.com
short_keyword: go
insecure_cookies: false
session_lifetime: 720h
```

## Session Lifetime Format

The `JOE_SESSION_LIFETIME` value uses Go's `time.Duration` format. Examples:

| Value | Duration |
|-------|----------|
| `720h` | 30 days (default) |
| `168h` | 7 days |
| `24h` | 1 day |
| `8760h` | 365 days |
| `1h30m` | 1 hour 30 minutes |

## Database DSN Examples

### SQLite

```
./joe-links.db
/var/lib/joe-links/joe-links.db
```

SQLite is the simplest option -- no external database server required. The file is created automatically on first run. Suitable for single-instance deployments.

### PostgreSQL

```
postgres://user:password@localhost:5432/joelinks?sslmode=disable
postgres://user:password@db.example.com:5432/joelinks?sslmode=require
```

Common query parameters:
- `sslmode=disable` -- no TLS (local development)
- `sslmode=require` -- require TLS (production)
- `sslmode=verify-full` -- require TLS with certificate verification

### MySQL

```
user:password@tcp(localhost:3306)/joelinks?parseTime=true
user:password@tcp(db.example.com:3306)/joelinks?parseTime=true&tls=true
```

:::warning
The `parseTime=true` parameter is required for MySQL. Without it, timestamp columns will not be parsed correctly.
:::
