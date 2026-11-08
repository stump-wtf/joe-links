// Governing: SPEC-0001 REQ "CLI Entrypoint", "Authentication Mode Selection", "Server-Side Sessions", ADR-0003, ADR-0004
// Governing: SPEC-0001 REQ "Local Admin Authentication", ADR-0024
// Governing: SPEC-0017 REQ "LLM Provider Configuration", ADR-0017
// Governing: SPEC-0021 REQ "Click Retention", ADR-0021
package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	HTTP struct {
		Addr string
	}
	DB struct {
		Driver string
		DSN    string
	}
	OIDC struct {
		Issuer       string
		ClientID     string
		ClientSecret string
		RedirectURL  string
	}
	AdminEmail string
	// AdminUsername and AdminPassword select and configure local mode, the
	// login path for deployments with no OIDC provider (behind an auth proxy,
	// or a single-operator homelab). AdminUsername defaults to
	// DefaultAdminUsername; AdminPassword is required in local mode and is
	// hashed at startup, never stored in plaintext. Both are ignored when OIDC
	// is configured, and configuring both login paths at once fails startup.
	// Governing: SPEC-0001 REQ "Local Admin Authentication", ADR-0024
	AdminUsername   string
	AdminPassword   string
	AdminGroups     []string // OIDC group names that grant the admin role
	GroupsClaim     string   // OIDC claim name containing the user's groups (default: "groups")
	ShortKeyword    string   // override the short keyword prefix (default: first label of HTTP host)
	SessionLifetime time.Duration
	InsecureCookies bool
	// TrustedProxies lists the CIDRs whose X-Forwarded-For value may be
	// believed when resolving the client IP (JOE_TRUSTED_PROXIES, or a
	// trusted_proxies list in joe-links.yaml). Empty, the default, believes no
	// caller-supplied header and keys on the address that opened the
	// connection. A bare IP is read as a single-address prefix, and an
	// unparsable entry fails startup rather than being dropped, because a typo
	// here would silently widen who may rewrite the observed client IP.
	// Governing: ADR-0023
	TrustedProxies []string
	LLM            struct {
		Provider string // "anthropic", "openai", or "openai-compatible"; empty = disabled
		APIKey   string
		Model    string
		BaseURL  string // override for openai-compatible providers
		Prompt   string // custom prompt template text (overrides built-in default)
	}
	// ClickRetentionDays is the click retention horizon in days
	// (JOE_CLICK_RETENTION). 0 (the default, unset) disables retention
	// entirely — no click row is ever deleted. Pruned rows are unrecoverable.
	// Governing: SPEC-0021 REQ "Click Retention", ADR-0021 (e)
	ClickRetentionDays int
	// Health configures the destination health checker goroutine.
	// Governing: SPEC-0020 REQ "Destination Health Checking", ADR-0020
	Health struct {
		Enabled      bool          // JOE_HEALTH_CHECKS_ENABLED (default true); false = goroutine never starts
		Interval     time.Duration // JOE_HEALTH_CHECK_INTERVAL (default 24h, minimum enforced 1h)
		Timeout      time.Duration // JOE_HEALTH_CHECK_TIMEOUT (default 10s, maximum enforced 30s)
		AllowPrivate bool          // JOE_HEALTH_CHECK_ALLOW_PRIVATE (default false) — operator-level SSRF escape hatch
	}
}

// Authentication modes selected by configuration (ADR-0024).
const (
	// AuthModeOIDC is selected when any JOE_OIDC_* setting is present. All of
	// them are then required, and no password is ever accepted.
	AuthModeOIDC = "oidc"
	// AuthModeLocal is selected when no JOE_OIDC_* setting is present: joe-links
	// serves with one local administrator account, authenticated against
	// AdminPassword. Self-registration does not exist in either mode — in local
	// mode the only account that can sign in is the configured administrator.
	AuthModeLocal = "local"

	// DefaultAdminUsername is the local-mode administrator username used when
	// JOE_ADMIN_USERNAME is unset.
	DefaultAdminUsername = "admin"

	// MinAdminPasswordLength is the floor for JOE_ADMIN_PASSWORD. Local mode is
	// the login path that may face the internet directly, so a password short
	// enough to guess online is rejected at startup rather than accepted and
	// logged in.
	MinAdminPasswordLength = 12
)

// AuthMode reports which login path this configuration selects. Any OIDC
// setting counts as an OIDC deployment: half-configured OIDC is a mistake to
// fail on, not a reason to fall back to a password nobody meant to enable.
// Governing: SPEC-0001 REQ "Authentication Mode Selection", ADR-0024
func (c *Config) AuthMode() string {
	if c.OIDC.Issuer != "" || c.OIDC.ClientID != "" || c.OIDC.ClientSecret != "" || c.OIDC.RedirectURL != "" {
		return AuthModeOIDC
	}
	return AuthModeLocal
}

// Load reads config from environment (JOE_ prefix) and optional joe-links.yaml.
func Load() (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix("JOE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	v.SetConfigName("joe-links")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	_ = v.ReadInConfig() // optional config file

	v.SetDefault("http.addr", ":8080")
	v.SetDefault("session.lifetime", "720h")
	// Governing: SPEC-0020 REQ "Destination Health Checking" — config defaults
	v.SetDefault("health_checks.enabled", true)
	v.SetDefault("health_check.interval", "24h")
	v.SetDefault("health_check.timeout", "10s")
	v.SetDefault("health_check.allow_private", false)

	cfg := &Config{}
	cfg.HTTP.Addr = v.GetString("http.addr")
	cfg.DB.Driver = v.GetString("db.driver")
	cfg.DB.DSN = v.GetString("db.dsn")
	cfg.OIDC.Issuer = v.GetString("oidc.issuer")
	cfg.OIDC.ClientID = v.GetString("oidc.client_id")
	cfg.OIDC.ClientSecret = v.GetString("oidc.client_secret")
	cfg.OIDC.RedirectURL = v.GetString("oidc.redirect_url")
	cfg.AdminEmail = v.GetString("admin_email")
	cfg.AdminUsername = strings.TrimSpace(v.GetString("admin_username"))
	cfg.AdminPassword = v.GetString("admin_password")
	cfg.InsecureCookies = v.GetBool("insecure_cookies")
	if raw := v.GetString("oidc.admin_groups"); raw != "" {
		for _, g := range strings.Split(raw, ",") {
			if g = strings.TrimSpace(g); g != "" {
				cfg.AdminGroups = append(cfg.AdminGroups, g)
			}
		}
	}
	cfg.GroupsClaim = v.GetString("oidc.groups_claim")
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	cfg.ShortKeyword = v.GetString("short_keyword")

	cfg.LLM.Provider = v.GetString("llm.provider")
	cfg.LLM.APIKey = v.GetString("llm.api_key")
	cfg.LLM.Model = v.GetString("llm.model")
	cfg.LLM.BaseURL = v.GetString("llm.base_url")
	cfg.LLM.Prompt = v.GetString("llm.prompt")

	lifetime, err := time.ParseDuration(v.GetString("session.lifetime"))
	if err != nil {
		return nil, fmt.Errorf("invalid JOE_SESSION_LIFETIME: %w", err)
	}
	cfg.SessionLifetime = lifetime

	// Health checker config. The interval floor and timeout ceiling are
	// normative politeness bounds and MUST NOT be relaxable by configuration,
	// so out-of-range values are clamped rather than honored.
	// Governing: SPEC-0020 REQ "Destination Health Checking", "Security
	// Requirements" — Rate Limiting and Abuse
	cfg.Health.Enabled = v.GetBool("health_checks.enabled")
	cfg.Health.AllowPrivate = v.GetBool("health_check.allow_private")
	interval, err := time.ParseDuration(v.GetString("health_check.interval"))
	if err != nil {
		return nil, fmt.Errorf("invalid JOE_HEALTH_CHECK_INTERVAL: %w", err)
	}
	if interval < time.Hour {
		interval = time.Hour
	}
	cfg.Health.Interval = interval
	timeout, err := time.ParseDuration(v.GetString("health_check.timeout"))
	if err != nil {
		return nil, fmt.Errorf("invalid JOE_HEALTH_CHECK_TIMEOUT: %w", err)
	}
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cfg.Health.Timeout = timeout

	// Click retention (JOE_CLICK_RETENTION, integer days). Unset or 0 disables
	// retention entirely — retention is off by default, because pruning is
	// irreversible deletion of source-of-truth rows. Negative or non-integer
	// values fail startup with a clear config error, and values 1–89 fail the
	// ≥90-day floor protecting SPEC-0020's staleness views, which compute a
	// 90-day window directly from link_clicks.
	// Governing: SPEC-0021 REQ "Click Retention", ADR-0021 (e)
	if raw := strings.TrimSpace(v.GetString("click_retention")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid JOE_CLICK_RETENTION %q: must be an integer number of days", raw)
		}
		if days < 0 {
			return nil, fmt.Errorf("invalid JOE_CLICK_RETENTION %d: must not be negative (0 or unset disables retention)", days)
		}
		if days > 0 && days < 90 {
			return nil, fmt.Errorf("invalid JOE_CLICK_RETENTION %d: must be at least 90 days — SPEC-0020's staleness views compute a 90-day window directly from link_clicks, and retention must not undercut it (SPEC-0021 REQ \"Click Retention\")", days)
		}
		cfg.ClickRetentionDays = days
	}

	// Client IP trust. Unset means nothing sits between the client and this
	// server, so no X-Forwarded-For value is believed. Entries are normalized
	// to canonical CIDR form so the router can test the connecting peer
	// against them directly.
	// Governing: ADR-0023
	for _, entry := range proxyListEntries(v.Get("trusted_proxies")) {
		prefix, err := parseProxyPrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid JOE_TRUSTED_PROXIES entry %q: must be a CIDR (127.0.0.1/32) or a bare IP (127.0.0.1)", entry)
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, prefix.String())
	}

	if cfg.DB.Driver == "" {
		return nil, fmt.Errorf("JOE_DB_DRIVER is required (sqlite3, mysql, postgres)")
	}
	if cfg.DB.DSN == "" {
		return nil, fmt.Errorf("JOE_DB_DSN is required")
	}

	// Login-path selection. OIDC stays the documented default path and its
	// requirements are unchanged; local mode is the opt-in for deployments with
	// no provider. Neither mode may be half-configured, and running both is
	// refused: a password beside a working IdP would be a second, weaker door
	// the operator did not choose.
	// Governing: SPEC-0001 REQ "Authentication Mode Selection", ADR-0024
	switch cfg.AuthMode() {
	case AuthModeOIDC:
		if cfg.OIDC.Issuer == "" {
			return nil, fmt.Errorf("JOE_OIDC_ISSUER is required: another JOE_OIDC_* setting is configured, and joe-links will not start with a partial OIDC setup")
		}
		if cfg.OIDC.ClientID == "" {
			return nil, fmt.Errorf("JOE_OIDC_CLIENT_ID is required")
		}
		if cfg.OIDC.ClientSecret == "" {
			return nil, fmt.Errorf("JOE_OIDC_CLIENT_SECRET is required")
		}
		if cfg.OIDC.RedirectURL == "" {
			return nil, fmt.Errorf("JOE_OIDC_REDIRECT_URL is required")
		}
		if cfg.AdminPassword != "" {
			return nil, fmt.Errorf("JOE_ADMIN_PASSWORD is ignored while OIDC is configured: remove it, or remove the JOE_OIDC_* settings to run in local mode")
		}
	default:
		if cfg.AdminUsername == "" {
			cfg.AdminUsername = DefaultAdminUsername
		}
		if cfg.AdminPassword == "" {
			return nil, fmt.Errorf("no login path is configured: set JOE_OIDC_ISSUER, JOE_OIDC_CLIENT_ID, JOE_OIDC_CLIENT_SECRET and JOE_OIDC_REDIRECT_URL for OIDC sign-in, or JOE_ADMIN_PASSWORD (optionally with JOE_ADMIN_USERNAME, default %q) for a local admin account", DefaultAdminUsername)
		}
		if len(cfg.AdminPassword) < MinAdminPasswordLength {
			return nil, fmt.Errorf("JOE_ADMIN_PASSWORD must be at least %d characters: local mode serves with this one account, and joe-links may be reachable from the internet", MinAdminPasswordLength)
		}
		if strings.ContainsAny(cfg.AdminUsername, " \t\n@") {
			return nil, fmt.Errorf("invalid JOE_ADMIN_USERNAME %q: use a plain name without spaces or @", cfg.AdminUsername)
		}
		if cfg.AdminEmail == "" {
			cfg.AdminEmail = cfg.AdminUsername + "@local"
		}
	}

	return cfg, nil
}

// proxyListEntries flattens the trusted_proxies setting into entries:
// JOE_TRUSTED_PROXIES arrives as one comma-separated string, a YAML
// trusted_proxies list arrives as a slice.
func proxyListEntries(raw any) []string {
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	switch v := raw.(type) {
	case string:
		for _, s := range strings.Split(v, ",") {
			add(s)
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	case []any:
		for _, e := range v {
			add(fmt.Sprint(e))
		}
	}
	return out
}

// parseProxyPrefix reads a CIDR, or a bare IPv4/IPv6 address as a
// single-address prefix, and returns it in canonical form.
func parseProxyPrefix(entry string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(entry); err == nil {
		return p, nil
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, err
	}
	addr = addr.Unmap()
	bits := 128
	if addr.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(addr, bits), nil
}
