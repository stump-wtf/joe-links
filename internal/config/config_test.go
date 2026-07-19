// Tests for the health-checker configuration: JOE_-prefixed viper keys with
// sane defaults, and the normative politeness bounds — the 1h interval floor
// and 30s timeout ceiling MUST NOT be relaxable by configuration, so
// out-of-range values are clamped rather than honored.
//
// Governing: SPEC-0020 REQ "Destination Health Checking", "Security
// Requirements" — Rate Limiting and Abuse, ADR-0020
package config

import (
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
