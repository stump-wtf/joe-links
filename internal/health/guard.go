// SSRF containment for the destination health checker: the checker is the
// first component that makes this server fetch user-supplied URLs, so every
// connection it opens is gated by a deny-by-default IP classifier enforced at
// dial time against the actual resolved address (a connection-control hook,
// not DNS pre-resolution — so DNS rebinding cannot bypass it, and every
// redirect hop is independently re-checked).
//
// Governing: SPEC-0020 "Security Requirements" — SSRF Resistance (Health Checker), ADR-0020 (d)
package health

import (
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

// errBlockedByPolicy marks a dial refused by the private-network classifier.
// It surfaces wrapped inside *net.OpError/*url.Error; errors.Is unwraps the
// chain, letting the checker record the link as skipped — never broken.
var errBlockedByPolicy = errors.New("destination is not checkable under current server policy")

// errTooManyRedirects marks a probe that exhausted the redirect cap, which is
// a failure (unlike a policy block).
var errTooManyRedirects = errors.New("stopped after 5 redirects")

// maxRedirects caps redirect following per probe.
// Governing: SPEC-0020 REQ "Destination Health Checking" — Probe semantics
const maxRedirects = 5

// nat64Prefix is the NAT64 well-known prefix 64:ff9b::/96 (RFC 6052): an
// IPv6 route to an embedded IPv4 address, refused so the classifier cannot be
// bypassed by wrapping a private v4 target in v6 clothing.
var nat64Prefix = func() *net.IPNet {
	_, n, err := net.ParseCIDR("64:ff9b::/96")
	if err != nil {
		panic("parse NAT64 prefix: " + err.Error())
	}
	return n
}()

// ipBlocked is the deny-by-default classifier. It is built on the standard
// library's address-class predicates plus the explicit CGNAT / "this
// network" / NAT64 ranges — not a hand-maintained CIDR literal list, which is
// itself the vulnerability. IPv4-mapped and IPv4-compatible IPv6 addresses
// are unmapped to their IPv4 form first (To4), so ::ffff:127.0.0.1
// classifies as loopback rather than slipping past IPv6-only range checks.
// Governing: SPEC-0020 "Security Requirements" — SSRF Resistance (Health Checker)
func ipBlocked(ip net.IP) bool {
	// Unmap before any range test: To4 returns the 4-byte form for IPv4,
	// IPv4-mapped (::ffff:a.b.c.d), and IPv4-compatible (::a.b.c.d) addresses.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// Standard-library predicates: loopback, RFC 1918 + unique-local
	// (IsPrivate covers fc00::/7 for IPv6), link-local unicast/multicast
	// (169.254.0.0/16 — including cloud metadata endpoints — and fe80::/10),
	// unspecified (0.0.0.0, ::), and all multicast.
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// "This network" 0.0.0.0/8.
		if v4[0] == 0 {
			return true
		}
		// CGNAT 100.64.0.0/10.
		if v4[0] == 100 && v4[1]&0xC0 == 64 {
			return true
		}
		// Limited broadcast 255.255.255.255.
		if v4.Equal(net.IPv4bcast) {
			return true
		}
		return false
	}
	// NAT64-mapped 64:ff9b::/96.
	return nat64Prefix.Contains(ip)
}

// dialControl returns the net.Dialer Control hook enforcing the classifier at
// dial time. The address argument is the RESOLVED ip:port the kernel is about
// to connect to — after DNS — so a hostname that re-resolves to a private
// address on a later connection is still refused.
// JOE_HEALTH_CHECK_ALLOW_PRIVATE=true disables the block deliberately and
// globally (an operator decision for homelab deployments that shortlink
// internal services); there is no per-link override.
// Governing: SPEC-0020 "Security Requirements" — SSRF Resistance (Health Checker)
func dialControl(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return errBlockedByPolicy
		}
		ip := net.ParseIP(host)
		if ip == nil || ipBlocked(ip) {
			return errBlockedByPolicy
		}
		return nil
	}
}

// newProbeClient builds the ONE shared SSRF-guarded client all probe traffic
// goes through: the initial HEAD, the GET fallback, and every redirect hop
// dial through the same guarded transport, so no request path can bypass the
// classifier — each connection is independently gated.
// Governing: SPEC-0020 "Security Requirements" — SSRF Resistance (Health Checker)
func newProbeClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: dialControl(allowPrivate),
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Transport: transport,
		// The configured per-request timeout bounds the whole probe including
		// redirects and body reads.
		// Governing: SPEC-0020 REQ "Destination Health Checking" — Probe semantics
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// A redirect to a non-http(s) scheme is never followed: the probe
			// terminates on the 3xx as a normal terminal response, not a failure.
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return http.ErrUseLastResponse
			}
			if len(via) >= maxRedirects {
				return errTooManyRedirects
			}
			return nil
		},
	}
}
