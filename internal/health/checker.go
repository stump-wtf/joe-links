// Package health implements the in-process destination health checker: a
// background goroutine started by `joe-links serve` (the click-writer /
// gauge-updater pattern — ADR-0016) that periodically probes link
// destinations and persists outcomes to the link_health table via the shared
// store layer. It is polite by construction — bounded concurrency, per-host
// spacing, exponential backoff, 429 deference — and SSRF-contained (guard.go).
//
// Governing: SPEC-0020 REQ "Destination Health Checking", ADR-0020 (c)/(d)
package health

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/joestump/joe-links/internal/build"
	"github.com/joestump/joe-links/internal/store"
)

// Politeness bounds. These are normative in SPEC-0020 and deliberately NOT
// configurable — the checker fetches other people's servers.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Politeness;
// "Security Requirements" — Rate Limiting and Abuse
const (
	// maxInFlight caps concurrent probes per cycle.
	maxInFlight = 4
	// defaultHostSpacing is the minimum gap between successive probes to the
	// same host.
	defaultHostSpacing = time.Second
	// maxBodyRead caps how much of a GET fallback body is read before the
	// rest is discarded (responses are never persisted).
	maxBodyRead = 64 << 10
	// maxErrLen bounds the stored error string — checker errors are
	// attacker-influenced (a malicious destination controls response data).
	maxErrLen = 512
)

// Config carries the viper-loaded checker settings (JOE_ prefix).
// Governing: SPEC-0020 REQ "Destination Health Checking"
type Config struct {
	Enabled      bool          // JOE_HEALTH_CHECKS_ENABLED
	Interval     time.Duration // JOE_HEALTH_CHECK_INTERVAL (floor 1h, enforced in config.Load)
	Timeout      time.Duration // JOE_HEALTH_CHECK_TIMEOUT (ceiling 30s, enforced in config.Load)
	AllowPrivate bool          // JOE_HEALTH_CHECK_ALLOW_PRIVATE
}

// Checker probes due link destinations and records outcomes.
type Checker struct {
	links  *store.LinkStore
	cfg    Config
	client *http.Client

	// hostSpacing is defaultHostSpacing in production; tests shrink it so
	// multi-link cycles against one httptest host stay fast.
	hostSpacing time.Duration

	hostMu   sync.Mutex
	hostNext map[string]time.Time
}

// New builds a Checker. The single guarded client is constructed once so ALL
// probe traffic shares one SSRF-guarded transport.
func New(links *store.LinkStore, cfg Config) *Checker {
	return &Checker{
		links:       links,
		cfg:         cfg,
		client:      newProbeClient(cfg.Timeout, cfg.AllowPrivate),
		hostSpacing: defaultHostSpacing,
		hostNext:    make(map[string]time.Time),
	}
}

// Run executes check cycles until ctx is cancelled (plus one cycle at startup
// — the per-link next_check_at gate makes that safe across restarts: links
// checked recently are simply not due). When the checker is disabled it
// returns immediately and no health data is ever written.
//
// Probe cadence is governed per link by next_check_at, NOT by the wake-up
// cadence: the loop wakes MORE often than the interval (pollInterval) because
// a healthy link's next_check_at lands one interval after probe COMPLETION —
// always a few seconds past the cycle that probed it. Waking only at interval
// granularity would skip such links at the next cycle and probe them once
// every TWO intervals; frequent wake-ups pick them up within minutes of
// falling due while issuing no extra traffic (a wake-up with nothing due
// probes nothing).
// Governing: SPEC-0020 REQ "Destination Health Checking" — Politeness
// ("Healthy links are checked once per interval"); scenario "Checker
// Disabled by Config"
func (c *Checker) Run(ctx context.Context) {
	if !c.cfg.Enabled {
		return
	}
	c.runCycle(ctx)
	ticker := time.NewTicker(pollInterval(c.cfg.Interval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runCycle(ctx)
		}
	}
}

// pollInterval derives the wake-up cadence from the check interval: a
// fraction of the interval, capped at five minutes so a link never waits
// meaningfully past its next_check_at, floored at one second as a safety
// bound for the tiny intervals tests use. With the production interval floor
// of one hour (config.Load) this is always five minutes.
func pollInterval(interval time.Duration) time.Duration {
	p := interval / 8
	if p > 5*time.Minute {
		p = 5 * time.Minute
	}
	if p < time.Second {
		p = time.Second
	}
	return p
}

// runCycle probes every currently due link with bounded concurrency.
func (c *Checker) runCycle(ctx context.Context) {
	now := time.Now().UTC()
	due, err := c.links.ListDueForHealthCheck(ctx, now)
	if err != nil {
		log.Printf("health checker: list due links: %v", err)
		return
	}

	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for _, link := range due {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(l *store.Link) {
			defer wg.Done()
			defer func() { <-sem }()
			c.checkOne(ctx, l)
		}(link)
	}
	wg.Wait()
}

// probeOutcome classifies a completed (or refused) probe.
type probeOutcome int

const (
	outcomeSuccess probeOutcome = iota
	outcomeFailure
	outcomeRateLimited
	outcomeSkipped
)

// probeResult is the classified result of one probe.
type probeResult struct {
	outcome    probeOutcome
	status     *int          // final HTTP status when one was received
	errMsg     string        // bounded failure/skip detail
	retryAfter time.Duration // parsed Retry-After on 429; 0 when absent
}

// checkOne probes a single link and persists the outcome. The store write
// keys off the outcome class: success resets the failure counter, failure
// increments it with backoff, 429 defers without touching it, and skipped
// marks the destination not-checkable without ever counting as broken.
func (c *Checker) checkOne(ctx context.Context, link *store.Link) {
	// Scheme allowlist backstop: rows predating the #265/#280 intake
	// validation may still carry non-http(s) destinations; they are recorded
	// as skipped without any fetch.
	// Governing: SPEC-0020 "Security Requirements" — SSRF Resistance
	if err := store.ValidateLinkURL(link.URL); err != nil {
		c.record(ctx, link, probeResult{outcome: outcomeSkipped, errMsg: truncateErr(err.Error())})
		return
	}

	c.waitHost(ctx, hostOf(link.URL))
	if ctx.Err() != nil {
		return
	}
	c.record(ctx, link, c.probe(ctx, link.URL))
}

// record persists a probe result via the store layer.
func (c *Checker) record(ctx context.Context, link *store.Link, res probeResult) {
	now := time.Now().UTC()
	var err error
	switch res.outcome {
	case outcomeSuccess:
		_, err = c.links.RecordHealthSuccess(ctx, link.ID, *res.status, now, c.cfg.Interval)
	case outcomeFailure:
		_, err = c.links.RecordHealthFailure(ctx, link.ID, res.status, res.errMsg, now, c.cfg.Interval)
	case outcomeRateLimited:
		// Push the next check out by at least one full interval, honoring
		// Retry-After when present and larger.
		// Governing: SPEC-0020 REQ "Destination Health Checking" scenario "429 Is Not a Failure"
		delay := c.cfg.Interval
		if res.retryAfter > delay {
			delay = res.retryAfter
		}
		_, err = c.links.DeferHealthCheck(ctx, link.ID, *res.status, now, delay)
	case outcomeSkipped:
		_, err = c.links.RecordHealthSkipped(ctx, link.ID, res.errMsg, now, c.cfg.Interval)
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("health checker: record result for %s: %v", link.Slug, err)
	}
}

// probe issues HEAD, falling back to GET on 405/501 (the resolver's own #196
// lesson, applied outbound). Both requests, and every redirect hop either
// follows, go through the single guarded client.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Probe semantics
func (c *Checker) probe(ctx context.Context, rawURL string) probeResult {
	res, retriable := c.doProbe(ctx, http.MethodHead, rawURL)
	if retriable {
		res, _ = c.doProbe(ctx, http.MethodGet, rawURL)
	}
	return res
}

// doProbe performs one HTTP request and classifies the outcome. The second
// return is true when the method was rejected (405/501) and a GET fallback
// should be attempted.
func (c *Checker) doProbe(ctx context.Context, method, rawURL string) (probeResult, bool) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return probeResult{outcome: outcomeFailure, errMsg: truncateErr(err.Error())}, false
	}
	// Identifying User-Agent so destination operators can attribute probes.
	// Governing: SPEC-0020 REQ "Destination Health Checking" — Probe semantics
	req.Header.Set("User-Agent", "joe-links-health/"+build.Version)

	resp, err := c.client.Do(req)
	if err != nil {
		return classifyProbeError(err), false
	}
	defer func() { _ = resp.Body.Close() }()
	// Read at most 64 KiB of any GET body before discarding; bodies are never
	// persisted (HEAD bodies are empty by definition).
	if method == http.MethodGet {
		_, _ = io.CopyN(io.Discard, resp.Body, maxBodyRead)
	}

	status := resp.StatusCode
	switch {
	case status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented:
		if method == http.MethodHead {
			return probeResult{}, true // GET fallback
		}
		return probeResult{outcome: outcomeFailure, status: &status}, false
	case status == http.StatusTooManyRequests:
		return probeResult{
			outcome:    outcomeRateLimited,
			status:     &status,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}, false
	case status >= 200 && status < 400:
		// A final 2xx or 3xx is a success — including a 3xx left unfollowed
		// because its Location was a non-http(s) scheme (terminal, not a failure).
		return probeResult{outcome: outcomeSuccess, status: &status}, false
	default:
		return probeResult{outcome: outcomeFailure, status: &status}, false
	}
}

// classifyProbeError maps a transport error onto an outcome: dials refused by
// the SSRF policy are skipped (blocked is never broken); everything else —
// timeouts, DNS failures, redirect-limit exhaustion — is a failure.
func classifyProbeError(err error) probeResult {
	if errors.Is(err, errBlockedByPolicy) {
		return probeResult{outcome: outcomeSkipped, errMsg: errBlockedByPolicy.Error()}
	}
	if errors.Is(err, errTooManyRedirects) {
		return probeResult{outcome: outcomeFailure, errMsg: errTooManyRedirects.Error()}
	}
	return probeResult{outcome: outcomeFailure, errMsg: truncateErr(err.Error())}
}

// parseRetryAfter parses a Retry-After header value: delta-seconds or an
// HTTP-date. Returns 0 when absent or unparseable.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// waitHost enforces the per-host spacing: each caller reserves the next
// probe slot for the host under the mutex, then sleeps until its slot —
// concurrent workers probing the same host serialize at hostSpacing apart.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Politeness
func (c *Checker) waitHost(ctx context.Context, host string) {
	c.hostMu.Lock()
	now := time.Now()
	slot := c.hostNext[host]
	if slot.Before(now) {
		slot = now
	}
	c.hostNext[host] = slot.Add(c.hostSpacing)
	c.hostMu.Unlock()

	wait := time.Until(slot)
	if wait <= 0 {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// hostOf extracts the hostname (without port) for per-host spacing.
func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Hostname()
	}
	return rawURL
}

// truncateErr bounds attacker-influenced error text before storage.
func truncateErr(s string) string {
	if len(s) > maxErrLen {
		return s[:maxErrLen]
	}
	return s
}
