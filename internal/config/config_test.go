// Tests for the health-checker configuration: JOE_-prefixed viper keys with
// sane defaults, and the normative politeness bounds — the 1h interval floor
// and 30s timeout ceiling MUST NOT be relaxable by configuration, so
// out-of-range values are clamped rather than honored.
//
// Governing: SPEC-0020 REQ "Destination Health Checking", "Security
// Requirements" — Rate Limiting and Abuse, ADR-0020
package config

import (
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
