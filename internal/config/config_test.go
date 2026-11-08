// Tests for the health-checker configuration: JOE_-prefixed viper keys with
// sane defaults, and the normative politeness bounds — the 1h interval floor
// and 30s timeout ceiling MUST NOT be relaxable by configuration, so
// out-of-range values are clamped rather than honored.
//
// Governing: SPEC-0020 REQ "Destination Health Checking", "Security
// Requirements" — Rate Limiting and Abuse, ADR-0020
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setRequiredEnv satisfies Load's mandatory settings so health-config
// assertions can run. t.Setenv also isolates and restores the environment.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JOE_DB_DRIVER", "sqlite3")
	t.Setenv("JOE_DB_DSN", ":memory:")
	t.Setenv("JOE_OIDC_ISSUER", "https://issuer.example.com")
	t.Setenv("JOE_OIDC_CLIENT_ID", "cid")
	t.Setenv("JOE_OIDC_CLIENT_SECRET", "secret")
	t.Setenv("JOE_OIDC_REDIRECT_URL", "https://go.example.com/auth/callback")
}

func TestHealthConfig_Defaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Health.Enabled {
		t.Error("JOE_HEALTH_CHECKS_ENABLED default = false, want true")
	}
	if cfg.Health.Interval != 24*time.Hour {
		t.Errorf("JOE_HEALTH_CHECK_INTERVAL default = %v, want 24h", cfg.Health.Interval)
	}
	if cfg.Health.Timeout != 10*time.Second {
		t.Errorf("JOE_HEALTH_CHECK_TIMEOUT default = %v, want 10s", cfg.Health.Timeout)
	}
	if cfg.Health.AllowPrivate {
		t.Error("JOE_HEALTH_CHECK_ALLOW_PRIVATE default = true, want false")
	}
}

func TestHealthConfig_EnvOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_HEALTH_CHECKS_ENABLED", "false")
	t.Setenv("JOE_HEALTH_CHECK_INTERVAL", "6h")
	t.Setenv("JOE_HEALTH_CHECK_TIMEOUT", "5s")
	t.Setenv("JOE_HEALTH_CHECK_ALLOW_PRIVATE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Health.Enabled {
		t.Error("Enabled = true, want false from JOE_HEALTH_CHECKS_ENABLED")
	}
	if cfg.Health.Interval != 6*time.Hour {
		t.Errorf("Interval = %v, want 6h", cfg.Health.Interval)
	}
	if cfg.Health.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", cfg.Health.Timeout)
	}
	if !cfg.Health.AllowPrivate {
		t.Error("AllowPrivate = false, want true from JOE_HEALTH_CHECK_ALLOW_PRIVATE")
	}
}

// The politeness bounds are normative and not relaxable: a sub-hour interval
// clamps to the 1h floor and an over-30s timeout clamps to the ceiling.
func TestHealthConfig_PolitenessBoundsAreClampedNotHonored(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_HEALTH_CHECK_INTERVAL", "5m")
	t.Setenv("JOE_HEALTH_CHECK_TIMEOUT", "5m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Health.Interval != time.Hour {
		t.Errorf("Interval = %v, want the 1h floor", cfg.Health.Interval)
	}
	if cfg.Health.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want the 30s ceiling", cfg.Health.Timeout)
	}
}

func TestHealthConfig_InvalidDurationsRejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_HEALTH_CHECK_INTERVAL", "often")
	if _, err := Load(); err == nil {
		t.Error("Load accepted an unparseable JOE_HEALTH_CHECK_INTERVAL")
	}
}

// Click-retention configuration (JOE_CLICK_RETENTION): off by default,
// integer days when set, with negative / non-integer / below-90-day values
// failing startup — the ≥90-day floor protects SPEC-0020's staleness views,
// which compute a 90-day window directly from link_clicks.
// Governing: SPEC-0021 REQ "Click Retention", ADR-0021 (e)

func TestClickRetentionConfig_DefaultOff(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClickRetentionDays != 0 {
		t.Errorf("JOE_CLICK_RETENTION default = %d, want 0 (retention off — no deletion)", cfg.ClickRetentionDays)
	}
}

func TestClickRetentionConfig_OptIn(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_CLICK_RETENTION", "365")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClickRetentionDays != 365 {
		t.Errorf("ClickRetentionDays = %d, want 365", cfg.ClickRetentionDays)
	}
}

func TestClickRetentionConfig_ExplicitZeroDisables(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_CLICK_RETENTION", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClickRetentionDays != 0 {
		t.Errorf("ClickRetentionDays = %d, want 0 (explicit 0 disables retention)", cfg.ClickRetentionDays)
	}
}

func TestClickRetentionConfig_NegativeFailsStartup(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_CLICK_RETENTION", "-30")
	if _, err := Load(); err == nil {
		t.Error("Load accepted a negative JOE_CLICK_RETENTION")
	}
}

func TestClickRetentionConfig_NonIntegerFailsStartup(t *testing.T) {
	setRequiredEnv(t)
	for _, v := range []string{"90d", "1.5", "forever"} {
		t.Setenv("JOE_CLICK_RETENTION", v)
		if _, err := Load(); err == nil {
			t.Errorf("Load accepted non-integer JOE_CLICK_RETENTION %q", v)
		}
	}
}

// Values 1–89 fail startup naming the staleness constraint: SPEC-0020's
// staleness views compute a 90-day window directly from link_clicks, and
// retention must not undercut it.
func TestClickRetentionConfig_BelowStalenessFloorFailsStartup(t *testing.T) {
	setRequiredEnv(t)
	for _, v := range []string{"1", "89"} {
		t.Setenv("JOE_CLICK_RETENTION", v)
		_, err := Load()
		if err == nil {
			t.Errorf("Load accepted JOE_CLICK_RETENTION=%s below the 90-day staleness floor", v)
			continue
		}
		if !strings.Contains(err.Error(), "90") || !strings.Contains(err.Error(), "staleness") {
			t.Errorf("floor error must name the 90-day staleness constraint; got %q", err)
		}
	}

	// The floor itself is allowed.
	t.Setenv("JOE_CLICK_RETENTION", "90")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load rejected JOE_CLICK_RETENTION=90: %v", err)
	}
	if cfg.ClickRetentionDays != 90 {
		t.Errorf("ClickRetentionDays = %d, want 90", cfg.ClickRetentionDays)
	}
}

// Client IP trust: the default believes no caller-supplied header, entries are
// normalized to canonical CIDRs for the router's peer check, and a typo fails
// startup instead of quietly widening who can rewrite the observed client IP.
//
// Governing: ADR-0023

func TestTrustedProxies_DefaultIsEmpty(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want empty: with no proxy configured no caller-supplied header may be believed", cfg.TrustedProxies)
	}
}

func TestTrustedProxies_NormalizedToCIDRs(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_TRUSTED_PROXIES", "127.0.0.1, 10.0.0.0/8, ::1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"127.0.0.1/32", "10.0.0.0/8", "::1/128"}
	if len(cfg.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
	}
	for i, w := range want {
		if cfg.TrustedProxies[i] != w {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.TrustedProxies[i], w)
		}
	}
}

func TestTrustedProxies_RejectsUnparsableEntries(t *testing.T) {
	setRequiredEnv(t)
	for _, v := range []string{"localhost", "10.0.0.0/33", "10.0.0.0/8, nope"} {
		t.Setenv("JOE_TRUSTED_PROXIES", v)
		_, err := Load()
		if err == nil {
			t.Errorf("Load accepted JOE_TRUSTED_PROXIES=%q", v)
			continue
		}
		if !strings.Contains(err.Error(), "JOE_TRUSTED_PROXIES") {
			t.Errorf("error must name the setting; got %q", err)
		}
	}
}

// The YAML form of the setting arrives as a list rather than the env form's
// one comma-separated string, so the list branch of the flattener needs its
// own proof that entries are normalized. Load reads joe-links.yaml from the
// working directory only, so the test runs in a temp dir holding one.
//
// Governing: ADR-0023
func TestTrustedProxies_YAMLListNormalizedToCIDRs(t *testing.T) {
	setRequiredEnv(t)
	dir := t.TempDir()
	cfgYAML := "trusted_proxies:\n  - 127.0.0.1\n  - 10.0.0.0/8\n"
	if err := os.WriteFile(filepath.Join(dir, "joe-links.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write joe-links.yaml: %v", err)
	}
	t.Chdir(dir)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"127.0.0.1/32", "10.0.0.0/8"}
	if len(cfg.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
	}
	for i, w := range want {
		if cfg.TrustedProxies[i] != w {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.TrustedProxies[i], w)
		}
	}
}

// clearOIDCEnv unsets the OIDC settings so a test can exercise local mode.
func clearOIDCEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"JOE_OIDC_ISSUER", "JOE_OIDC_CLIENT_ID", "JOE_OIDC_CLIENT_SECRET", "JOE_OIDC_REDIRECT_URL"} {
		t.Setenv(k, "")
	}
}

func setLocalModeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JOE_DB_DRIVER", "sqlite3")
	t.Setenv("JOE_DB_DSN", ":memory:")
	clearOIDCEnv(t)
}

func TestAuthMode_OIDCWhenConfigured(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthMode() != AuthModeOIDC {
		t.Errorf("AuthMode() = %q, want %q", cfg.AuthMode(), AuthModeOIDC)
	}
}

func TestAuthMode_LocalNeedsOnlyAdminPassword(t *testing.T) {
	setLocalModeEnv(t)
	t.Setenv("JOE_ADMIN_PASSWORD", "a-long-enough-password")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthMode() != AuthModeLocal {
		t.Errorf("AuthMode() = %q, want %q", cfg.AuthMode(), AuthModeLocal)
	}
	if cfg.AdminUsername != DefaultAdminUsername {
		t.Errorf("AdminUsername = %q, want the default %q", cfg.AdminUsername, DefaultAdminUsername)
	}
	if cfg.AdminEmail != "admin@local" {
		t.Errorf("AdminEmail = %q, want the local default admin@local", cfg.AdminEmail)
	}
}

func TestAuthMode_LocalHonorsCustomAdmin(t *testing.T) {
	setLocalModeEnv(t)
	t.Setenv("JOE_ADMIN_USERNAME", "  joestump  ")
	t.Setenv("JOE_ADMIN_PASSWORD", "a-long-enough-password")
	t.Setenv("JOE_ADMIN_EMAIL", "joe@example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminUsername != "joestump" {
		t.Errorf("AdminUsername = %q, want the trimmed joestump", cfg.AdminUsername)
	}
	if cfg.AdminEmail != "joe@example.com" {
		t.Errorf("AdminEmail = %q, want the configured address", cfg.AdminEmail)
	}
}

func TestAuthMode_FailsWhenNeitherPathIsConfigured(t *testing.T) {
	setLocalModeEnv(t)

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded with no login path configured, want a startup failure")
	}
	for _, want := range []string{"JOE_OIDC_ISSUER", "JOE_ADMIN_PASSWORD", DefaultAdminUsername} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestAuthMode_FailsOnGuessableAdminPassword(t *testing.T) {
	setLocalModeEnv(t)
	t.Setenv("JOE_ADMIN_PASSWORD", "shorty")

	if _, err := Load(); err == nil {
		t.Fatalf("Load succeeded with a %d-character admin password, want a startup failure", len("shorty"))
	}
}

func TestAuthMode_FailsOnMalformedAdminUsername(t *testing.T) {
	setLocalModeEnv(t)
	t.Setenv("JOE_ADMIN_USERNAME", "joe@example.com")
	t.Setenv("JOE_ADMIN_PASSWORD", "a-long-enough-password")

	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded with an email address as the username, want a startup failure")
	}
}

func TestAuthMode_FailsOnPartialOIDCConfig(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_OIDC_CLIENT_SECRET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded with a partial OIDC setup, want a startup failure")
	}
	if !strings.Contains(err.Error(), "JOE_OIDC_CLIENT_SECRET") {
		t.Errorf("error %q does not name the missing setting", err)
	}
}

// Both login paths at once is refused: beside a working provider a password
// would be a second, weaker door the operator never chose.
func TestAuthMode_FailsWhenBothPathsAreConfigured(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("JOE_ADMIN_PASSWORD", "a-long-enough-password")

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded with OIDC and a local admin password, want a startup failure")
	}
	if !strings.Contains(err.Error(), "JOE_ADMIN_PASSWORD") {
		t.Errorf("error %q does not name the conflicting setting", err)
	}
}
