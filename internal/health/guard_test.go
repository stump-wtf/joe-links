// Tests for the checker's SSRF containment: the deny-by-default IP
// classifier, dial-time enforcement, the scheme allowlist backstop, the
// redirect cap, and the skipped-never-broken recording of blocked
// destinations.
//
// Governing: SPEC-0020 "Security Requirements" — SSRF Resistance (Health Checker), ADR-0020 (d)
package health

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/store"
)

// The classifier refuses every private, local, and special-purpose range and
// unmaps IPv4-mapped IPv6 addresses before any range test, so
// ::ffff:127.0.0.1 classifies as loopback.
func TestSSRFGuard_ClassifierRefusesPrivateAndSpecialRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",        // loopback
		"::1",              // IPv6 loopback
		"::ffff:127.0.0.1", // IPv4-mapped loopback — must be unmapped first
		"10.1.2.3",         // RFC 1918
		"172.16.0.1",       // RFC 1918
		"192.168.1.1",      // RFC 1918
		"::ffff:10.1.2.3",  // IPv4-mapped RFC 1918
		"169.254.169.254",  // link-local — cloud metadata endpoint
		"fe80::1",          // IPv6 link-local
		"fc00::1",          // unique-local
		"fd12:3456::1",     // unique-local
		"100.64.0.1",       // CGNAT
		"100.127.255.254",  // CGNAT upper edge
		"0.0.0.0",          // unspecified
		"::",               // IPv6 unspecified
		"0.1.2.3",          // "this network" 0.0.0.0/8
		"255.255.255.255",  // limited broadcast
		"224.0.0.251",      // multicast
		"ff02::fb",         // IPv6 multicast
		"64:ff9b::7f00:1",  // NAT64-mapped
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !ipBlocked(ip) {
			t.Errorf("ipBlocked(%s) = false, want true", s)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700::1111", "100.128.0.1"}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if ipBlocked(ip) {
			t.Errorf("ipBlocked(%s) = true, want false (public address)", s)
		}
	}
}

// Enforcement happens at dial time on the resolved address: a probe of a
// loopback destination is refused before any connection is made, and the
// outcome is recorded as skipped — never broken (unchecked is not dead).
func TestSSRFGuard_BlockedDestinationRecordedSkippedNeverBroken(t *testing.T) {
	env := newCheckerEnv(t)
	srv, count := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	link := env.createLink(t, "internal", srv.URL) // 127.0.0.1 — blocked by default

	cfg := allowPrivateCfg(time.Hour)
	cfg.AllowPrivate = false // the default posture
	c := env.newChecker(cfg)
	c.runCycle(context.Background())

	if got := count.Load(); got != 0 {
		t.Fatalf("server received %d requests, want 0 — the dial must be refused", got)
	}
	h := env.health(t, link.ID)
	if h == nil {
		t.Fatal("no link_health row — a blocked destination is recorded as skipped")
	}
	if !h.Skipped {
		t.Error("skipped = false, want true")
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0 — blocked is never a strike", h.ConsecutiveFailures)
	}
	view := store.DeriveHealth(link, h, time.Now().UTC())
	if view.Status != store.HealthSkipped {
		t.Errorf("derived health = %q, want %q", view.Status, store.HealthSkipped)
	}
}

// JOE_HEALTH_CHECK_ALLOW_PRIVATE=true is the operator-level escape hatch for
// homelab deployments that shortlink internal services: the same loopback
// destination probes normally.
func TestSSRFGuard_AllowPrivateEnablesInternalDestinations(t *testing.T) {
	env := newCheckerEnv(t)
	srv, count := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	link := env.createLink(t, "homelab", srv.URL)

	c := env.newChecker(allowPrivateCfg(time.Hour))
	c.runCycle(context.Background())

	if got := count.Load(); got != 1 {
		t.Fatalf("probe count = %d, want 1", got)
	}
	h := env.health(t, link.ID)
	if h == nil || h.Skipped || h.LastStatus == nil || *h.LastStatus != http.StatusOK {
		t.Fatalf("health row = %+v, want a successful 200 probe", h)
	}
}

// Non-http(s) destinations are recorded as skipped without any fetch — the
// scheme allowlist backstop for rows predating intake validation (#280).
func TestSSRFGuard_NonHTTPSchemeRecordedSkippedWithoutFetch(t *testing.T) {
	env := newCheckerEnv(t)
	link := env.createLink(t, "legacy-ftp", "https://example.com/replaced-below")
	// Bypass intake validation the way a legacy row would: raw SQL.
	if _, err := env.db.Exec(env.db.Rebind(`UPDATE links SET url = ? WHERE id = ?`), "ftp://example.com/file", link.ID); err != nil {
		t.Fatalf("plant legacy scheme: %v", err)
	}

	cfg := allowPrivateCfg(time.Hour)
	cfg.AllowPrivate = false
	c := env.newChecker(cfg)
	c.runCycle(context.Background())

	h := env.health(t, link.ID)
	if h == nil {
		t.Fatal("no link_health row — a non-http(s) destination is recorded as skipped")
	}
	if !h.Skipped {
		t.Error("skipped = false, want true")
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", h.ConsecutiveFailures)
	}
	if h.LastStatus != nil {
		t.Errorf("last_status = %v, want nil — no fetch was issued", h.LastStatus)
	}
}

// Redirect-limit exhaustion is a failure (unlike a policy block): the probe
// follows at most 5 redirects.
func TestSSRFGuard_RedirectCapExhaustionIsFailure(t *testing.T) {
	env := newCheckerEnv(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	link := env.createLink(t, "redirect-loop", srv.URL)

	c := env.newChecker(allowPrivateCfg(time.Hour))
	c.runCycle(context.Background())

	h := env.health(t, link.ID)
	if h == nil {
		t.Fatal("no link_health row")
	}
	if h.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d, want 1", h.ConsecutiveFailures)
	}
	if h.Skipped {
		t.Error("skipped = true, want false — redirect exhaustion is a failure, not a policy block")
	}
	if h.LastError == nil || !strings.Contains(*h.LastError, "redirect") {
		t.Errorf("last_error = %v, want a redirect-limit message", h.LastError)
	}
}

// A redirect to a non-http(s) scheme is never followed: the probe terminates
// on the 3xx as a normal terminal response, not a failure.
func TestSSRFGuard_RedirectToNonHTTPSchemeTerminatesProbe(t *testing.T) {
	env := newCheckerEnv(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "ftp://internal.example.com/file")
		w.WriteHeader(http.StatusFound)
	})
	link := env.createLink(t, "scheme-hop", srv.URL)

	c := env.newChecker(allowPrivateCfg(time.Hour))
	c.runCycle(context.Background())

	h := env.health(t, link.ID)
	if h == nil {
		t.Fatal("no link_health row")
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0 — a terminal 3xx is a success", h.ConsecutiveFailures)
	}
	if h.LastStatus == nil || *h.LastStatus != http.StatusFound {
		t.Errorf("last_status = %v, want 302 (the unfollowed redirect)", h.LastStatus)
	}
}
