---
status: proposed
date: 2026-10-19
decision-makers: proposed by joestump-agent, awaiting joestump
extends: [ADR-0016]
---

# ADR-0022: Client IP Trust Model

## Context and Problem Statement

The router installs chi's `middleware.RealIP`, which is deprecated upstream (GHSA-3fxj-6jh8-hvhx). It rewrites `r.RemoteAddr` from `True-Client-IP`, `X-Real-IP`, or the leftmost `X-Forwarded-For` entry, and it does so **whether or not our infrastructure put those headers there**. Any caller can therefore choose the address this instance believes the request came from.

Two things read that address today:

* chi's request logger, so the access log records whatever the caller claimed.
* Click analytics (ADR-0016 / SPEC-0016), where `ip_hash = SHA-256(clientIP + ":" + dailySalt)` is the only identifier for an anonymous click. A caller can mint fresh "visitors" at will by rotating a fake header value, inflating the unique-visitor counts on any link, and can make its own clicks land in whatever bucket it likes.

Whatever we decide here also governs the next consumer. An IP-keyed rate limiter or abuse control (the topic SPEC-0020's security requirements point at) would inherit this resolution unchanged, so a decision that is merely adequate for analytics is a liability for that future feature.

Nothing in the repo configures which hop to believe: there is no trusted-proxy setting, so today the answer is "whoever sends the header first".

## Decision Drivers

* **Analytics must survive a proxy.** joe-links normally runs behind a reverse proxy (Caddy here). If every click hashes to the proxy's own address, every link has exactly one visitor, and ADR-0016's whole point is lost. A "secure" setting that quietly destroys the feature will get turned straight back off again.
* **Safe by default.** This is self-hosted software; the out-of-the-box install must not be spoofable just because the operator never read a docs page.
* **One resolution per request, at one place.** Every consumer must see the same answer, and no consumer should be able to reach for a header on its own.
* **No new dependency.** ADR-0001's dependency-light stance; chi already ships the pieces (its `ClientIPFrom*` middlewares exist precisely to replace `RealIP`), and the pinned version has them.
* **Configuration that fails loudly.** A typo in a trust list decides who can rewrite the observed IP. It must stop startup, not be dropped in silence.

## Considered Options

* **A. Drop header trust entirely; always key on the TCP peer address.** Nothing is spoofable. Rejected as the whole answer: behind any proxy the recorded IP is the proxy's, so anonymous-click attribution collapses to one visitor per link with no error anywhere to tell the operator.
* **B. Read `X-Forwarded-For`, skipping configured trusted CIDRs** (chi's `ClientIPFromXFF`). Correct behind a proxy whose ranges you can name. On its own it never looks at who actually connected: a client that reaches the app directly, bypassing the proxy, is believed when it sends its own `X-Forwarded-For`.
* **C. Read `X-Forwarded-For` by hop count** (chi's `ClientIPFromXFFTrustedProxies`). One number instead of CIDRs. Rejected: an off-by-one in the wrong direction is silent spoofability, and it assumes the network layer already lets only proxies reach us.
* **D. Trust a single header the proxy overwrites** (chi's `ClientIPFromHeader`, e.g. `X-Real-IP`). Simple, but only as safe as the proxy's guarantee that the header is always overwritten. Caddy sends `X-Forwarded-For` by default and does not guarantee `X-Real-IP`; the docs guide even tells nginx operators to set it by hand. That makes the safety of the product depend on each operator's proxy config.
* **E. Keep `RealIP` and tell operators to strip inbound headers at the proxy.** The fix lives outside the product, and the default install stays spoofable. Rejected.

## Decision Outcome

Chosen: **B, gated on the immediate peer** (option B alone is insufficient; the gate is what makes it safe to default).

1. **`RealIP` is removed.** `r.RemoteAddr` is never rewritten by joe-links, so no code path can inherit a caller-supplied value by accident.
2. **The client IP is resolved once, per request, onto the request context**, by a middleware chosen at startup:
   * `trusted_proxies` empty (the default): the client IP is the address that opened the connection. No header is consulted.
   * `trusted_proxies` set: the header is consulted **only if the peer that opened the connection falls inside one of those CIDRs**. Then `X-Forwarded-For` is walked right to left, skipping entries that are themselves trusted; the first untrusted entry is the client. If the peer is not in the list the header is ignored entirely, so a client that bypasses the proxy cannot be believed.
   * If resolution yields nothing (no header, or an unparseable chain), consumers fall back to the peer address, which no caller can forge. The fallback lives at the read site, not in the middleware, so there is exactly one resolution rule.
3. **Configuration is `trusted_proxies` / `JOE_TRUSTED_PROXIES`**, a list of CIDRs (a bare address is read as a single-address prefix), normalized at load time to canonical CIDR form. An entry that does not parse is a startup error.
4. **Consumers read the resolved value** (`middleware.GetClientIP`) and never a header. Click analytics is the one consumer today.

For this deployment that means: Caddy on the same host sets `trusted_proxies: ["127.0.0.1/32", "::1/128"]` (plus whatever address Caddy actually connects from), and clicks keep attributing to real visitors while a spoofed header changes nothing.

## Consequences

**Positive**

* A caller cannot choose the address its click is recorded under, in the default configuration or in a configured one, and the router tests prove it.
* The safe behavior needs no configuration; the setting only *adds* fidelity behind a proxy.
* An IP-keyed rate limiter added later reads the same honest value.

**Negative / costs**

* **Behavior change for operators:** the app's own access log now shows the connecting address (the proxy's), not the client's. The proxy's access log still carries the real client IP, and analytics still attributes correctly when `trusted_proxies` is set, but anyone grepping joe-links' log for a visitor IP will need that setting configured.
* An operator behind a proxy who does not set the list gets single-visitor analytics: the same collapse option A would have forced, but now it is a visible choice in their config file rather than a silent property of the code.
* Two hops of proxy (CDN then LB) are only correct when both ranges are listed; a missing hop means the CDN's address is recorded, not a client-controlled one. Fail-safe, not fail-spoofable.
* `X-Real-IP` is never believed. Operators following the old nginx guidance in the deployment guide must switch to `X-Forwarded-For` or accept the proxy's own address.

**To confirm before this is accepted**

* Whether the resolved client IP should also be emitted in the app's access log line (this ADR says no for now: the proxy already logs it, and mutating `RemoteAddr` to get it there is what we just removed).

## More Information

* chi's advisory and replacement API: GHSA-3fxj-6jh8-hvhx, `middleware.ClientIPFromXFF` / `GetClientIP`.
* Reported as issue #321; implemented in the PR that carries this ADR.
* Follow-up outside this repo: the Ansible service definition for joe-links must set `JOE_TRUSTED_PROXIES` for the hop Caddy actually connects from, or this deployment loses per-visitor attribution.
