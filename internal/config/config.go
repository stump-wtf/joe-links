// Governing: SPEC-0001 REQ "CLI Entrypoint", "OIDC-Only Authentication", "Server-Side Sessions", ADR-0003, ADR-0004
// Governing: SPEC-0017 REQ "LLM Provider Configuration", ADR-0017
package config

import (
	"fmt"
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
	AdminEmail      string
	AdminGroups     []string // OIDC group names that grant the admin role
	GroupsClaim     string   // OIDC claim name containing the user's groups (default: "groups")
	ShortKeyword    string   // override the short keyword prefix (default: first label of HTTP host)
	SessionLifetime time.Duration
	InsecureCookies bool
	LLM             struct {
		Provider string // "anthropic", "openai", or "openai-compatible"; empty = disabled
		APIKey   string
		Model    string
		BaseURL  string // override for openai-compatible providers
		Prompt   string // custom prompt template text (overrides built-in default)
	}
	// Health configures the destination health checker goroutine.
	// Governing: SPEC-0020 REQ "Destination Health Checking", ADR-0020
	Health struct {
		Enabled      bool          // JOE_HEALTH_CHECKS_ENABLED (default true); false = goroutine never starts
		Interval     time.Duration // JOE_HEALTH_CHECK_INTERVAL (default 24h, minimum enforced 1h)
		Timeout      time.Duration // JOE_HEALTH_CHECK_TIMEOUT (default 10s, maximum enforced 30s)
		AllowPrivate bool          // JOE_HEALTH_CHECK_ALLOW_PRIVATE (default false) — operator-level SSRF escape hatch
	}
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

	if cfg.DB.Driver == "" {
		return nil, fmt.Errorf("JOE_DB_DRIVER is required (sqlite3, mysql, postgres)")
	}
	if cfg.DB.DSN == "" {
		return nil, fmt.Errorf("JOE_DB_DSN is required")
	}
	if cfg.OIDC.Issuer == "" {
		return nil, fmt.Errorf("JOE_OIDC_ISSUER is required")
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

	return cfg, nil
}
