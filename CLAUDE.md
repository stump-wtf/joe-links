# joe-links

A self-hosted "go links" service — short memorable slugs that redirect to long URLs.
Go + HTMX + DaisyUI/Tailwind. Single binary.

## Architecture Context

- Architecture Decision Records: `docs/adrs/`
- Specifications: `docs/openspec/specs/`
- Governing spec: `docs/openspec/specs/joe-links-web-app/spec.md` (SPEC-0001)

## Stack

- **Language**: Go (module: `github.com/joestump/joe-links`)
- **CLI**: cobra + viper (`JOE_` env var prefix via `SetEnvPrefix("JOE")`)
- **Router**: `go-chi/chi`
- **Frontend**: HTMX + DaisyUI + Tailwind CSS
- **Templates**: `html/template` with `go:embed`
- **Database**: `sqlx` + `goose` migrations, drivers: `sqlite3` / `mysql` / `postgres`
- **Auth**: `coreos/go-oidc` + `golang.org/x/oauth2` + `alexedwards/scs` sessions

## Environment Variables (all `JOE_` prefixed)

| Variable | Default | Purpose |
|----------|---------|---------|
| `JOE_HTTP_ADDR` | `:8080` | HTTP bind address |
| `JOE_DB_DRIVER` | — | `sqlite3`, `mysql`, or `postgres` |
| `JOE_DB_DSN` | — | Database connection string |
| `JOE_OIDC_ISSUER` | — | OIDC provider discovery URL |
| `JOE_OIDC_CLIENT_ID` | — | OAuth2 client ID |
| `JOE_OIDC_CLIENT_SECRET` | — | OAuth2 client secret |
| `JOE_OIDC_REDIRECT_URL` | — | Callback URL (e.g. `https://joe.example.com/auth/callback`) |
| `JOE_ADMIN_EMAIL` | — | Email granted `admin` role on any login (grant-only; never demotes) |
| `JOE_OIDC_ADMIN_GROUPS` | — | Comma-separated OIDC group names that grant the `admin` role |
| `JOE_OIDC_GROUPS_CLAIM` | `groups` | OIDC claim name containing the user's groups |
| `JOE_SHORT_KEYWORD` | *(hostname first label)* | Override the short-link prefix shown in the UI (e.g. `go`); defaults to the first DNS label of the server hostname |
| `JOE_SESSION_LIFETIME` | `720h` | Session absolute expiry (30 days) |
| `JOE_LLM_PROVIDER` | *(unset — disabled)* | LLM metadata suggestions: `anthropic`, `openai`, or `openai-compatible` |
| `JOE_LLM_API_KEY` | — | API key for the chosen LLM provider |
| `JOE_LLM_MODEL` | *(provider default)* | LLM model name (e.g. `claude-haiku-4-5-20251001`, `gpt-4o-mini`, `llama3`) |
| `JOE_LLM_BASE_URL` | *(provider default)* | Base URL override for Ollama or any OpenAI-compatible endpoint |
| `JOE_LLM_PROMPT` | *(built-in default)* | Override the system prompt sent to the LLM |
| `JOE_INSECURE_COOKIES` | `false` | When `true`, disables the `Secure` flag on session/auth cookies so login works over plain HTTP (local development only — never enable in production) |

## Key Conventions

- All config is loaded via viper — **no direct `os.Getenv` calls** outside `internal/config/`
- HTMX partials: check `r.Header.Get("HX-Request")` and render fragment vs full page
- Governing comments in code: `// Governing: SPEC-0001 REQ "Short Link Resolution", ADR-0002`
- Slugs: `[a-z0-9][a-z0-9\-]*[a-z0-9]` — globally unique, reserved slugs (exact match): `admin`, `api`, `auth`, `dashboard`, `links`, `mcp`, `metrics`, `static`, `u` — defined in `internal/store/validate.go` (`store.ReservedSlugs()`)
- Sessions store only `user_id` (UUID) and `role` — no raw OIDC claims

## Commands

```bash
joe-links serve    # run migrations + start HTTP server
joe-links migrate  # run migrations and exit
```

## Release Process

Always use `gh release` when tagging releases — never push a bare tag without release notes.

The CI auto-creates a minimal release when a tag is pushed. Update it with proper notes immediately after — but **wait for the tag build to create the release first** (release-notes race): run `gh run watch` between `git push origin vX.Y.Z` and `gh release edit`, or the edit 404s / gets clobbered by the CI-created release.

```bash
git tag vX.Y.Z && git push origin vX.Y.Z
gh run watch   # wait for the tag build to create the release
gh release edit vX.Y.Z --notes "$(cat <<'EOF'
## Summary line

### Category
- Bullet points describing changes

**Full Changelog**: https://github.com/joestump/joe-links/compare/vX.Y.W...vX.Y.Z
EOF
)"
```

## Integrations

Third-party integrations live under `integrations/` to avoid polluting the repo root:

- `integrations/extension/` — Manifest V3 browser extension (Chrome, Firefox)
- `integrations/apple/` — Safari Web Extension Xcode project (iOS 15+, macOS 12+); future Apple-platform apps go here too

## SDD Skills

| Skill | Purpose |
|-------|---------|
| `/sdd:adr` | Create a new Architecture Decision Record |
| `/sdd:spec` | Create a new specification |
| `/sdd:check` | Quick-check code against ADRs and specs for drift |
| `/sdd:audit` | Comprehensive design artifact alignment audit |
| `/sdd:prime` | Load architecture context into session |

### SDD Configuration

- **Tracker**: GitHub
- **Owner**: joestump
- **Repo**: joe-links

#### Branch Conventions
- Prefix: `feature`
- Epic Prefix: `epic`
- Slug Max Length: 50

#### PR Conventions
- Close Keyword: `Closes`
- Ref Keyword: `Part of`
- Include Spec Reference: true

#### Projects
- Default Mode: per-epic
