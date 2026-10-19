// Client IP resolution for the whole router.
//
// Chi's middleware.RealIP is deprecated: it rewrites r.RemoteAddr from
// True-Client-IP, X-Real-IP or the leftmost X-Forwarded-For entry whether or
// not the infrastructure actually set them, so any caller could rewrite its
// apparent IP (GHSA-3fxj-6jh8-hvhx). Instead the client IP is resolved once per
// request from a source the operator configured, and stored on the request
// context for consumers (click analytics today) to read with
// middleware.GetClientIP. r.RemoteAddr is never mutated, so no code path can
// inherit a caller-supplied value by accident.
//
// Governing: ADR-0023
package handler

import (
	"net"
	"net/http"
	"net/netip"

	"github.com/go-chi/chi/v5/middleware"
)

// clientIPMiddleware returns the middleware that resolves the client IP.
//
// With no trusted proxies nothing is believed: the client IP is the address
// that opened the connection. With trusted proxies, X-Forwarded-For is walked
// right to left skipping those CIDRs, but only when the connecting peer is
// itself one of them. That peer check is what chi's ClientIPFromXFF alone does
// not do: without it, a client that reaches this server directly can put its
// own fake hop in the header and be believed.
//
// When the chain yields nothing (absent, or skipped as unparseable) no client
// IP is stored, and the read site falls back to the peer address, which no
// caller can forge.
func clientIPMiddleware(trustedProxies []string) func(http.Handler) http.Handler {
	if len(trustedProxies) == 0 {
		return middleware.ClientIPFromRemoteAddr
	}
	prefixes := parseProxyPrefixes(trustedProxies)
	fromXFF := middleware.ClientIPFromXFF(trustedProxies...)
	return func(h http.Handler) http.Handler {
		xffHandler := fromXFF(h)
		peerHandler := middleware.ClientIPFromRemoteAddr(h)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if peer, ok := peerAddr(r.RemoteAddr); ok && inAnyPrefix(peer, prefixes) {
				xffHandler.ServeHTTP(w, r)
				return
			}
			peerHandler.ServeHTTP(w, r)
		})
	}
}

// clientIP returns the client IP for consumers such as the analytics IP hash.
// It reads only what clientIPMiddleware stored on the request context, falling
// back to the address that opened the connection when header resolution gave
// nothing. It never reads a header itself, so a caller cannot choose the IP
// this request is recorded under.
//
// Governing: ADR-0023
func clientIP(r *http.Request) string {
	if ip := middleware.GetClientIP(r.Context()); ip != "" {
		return ip
	}
	if addr, ok := peerAddr(r.RemoteAddr); ok {
		return addr.String()
	}
	return r.RemoteAddr
}

// parseProxyPrefixes parses the operator's trusted-proxy CIDRs. config.Load
// validates and normalizes them at startup, so a failure here means a caller
// bypassed config; panicking matches chi, which panics on the same input.
func parseProxyPrefixes(entries []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			panic("invalid trusted proxy CIDR " + entry + ": " + err.Error())
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// peerAddr returns the address that opened the connection, port stripped and
// v4-mapped IPv6 folded to v4 so one logical client maps to one key.
func peerAddr(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // RemoteAddr may be a bare IP, as in some tests.
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func inAnyPrefix(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
