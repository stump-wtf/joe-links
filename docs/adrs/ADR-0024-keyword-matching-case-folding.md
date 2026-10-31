---
status: proposed
date: 2026-10-30
decision-makers: proposed by joestump-agent, awaiting joestump
extends: [ADR-0011]
---

# ADR-0024: Keyword Matching Is Case-Folded Like Slug Resolution

## Context and Problem Statement

PR #281 (issue #267) made slug resolution case-insensitive: the resolver lowercases each
candidate before `GetBySlug`, because slug validation rejects uppercase and the stored corpus is
therefore canonical. That work deliberately froze keyword behaviour, and SPEC-0019 recorded the
result as a known asymmetry: keyword lookup (`KeywordStore.GetByKeyword`) received the raw host or
first path segment and compared it exactly.

The two rules now disagree on the same URL. Admin keyword names are not reserved slugs, so a user
can own the link slug `gh` while `gh` is also a registered keyword. Keyword routing is evaluated
before slug resolution, so `/gh/joestump` follows the keyword template while `/GH/joestump` misses
the keyword lookup and falls through to the case-folded slug link. The case of one URL segment
decides which resource the request reaches, and it decides it differently per driver: MySQL's
default collation already compares case-insensitively, while sqlite3 and PostgreSQL do not, so the
divergence only shows up on some deployments.

## Decision Drivers

* One URL must reach one destination regardless of case — the resolver already promises this for
  slugs (SPEC-0019 REQ "Case-Insensitive Slug Resolution")
* Behaviour must be identical on sqlite3, MySQL and PostgreSQL (ADR-0002 pluggable database)
* DNS hostnames are case-insensitive, so a host arriving in mixed case is the same host
* The extension side already folds keywords: SPEC-0008 REQ "Keyword Host Discovery" requires the
  discovered keyword to be "matched case-insensitively"; the server was the odd one out
* No schema or collation change (ADR-0002), and resolution precedence must not shift

## Considered Options

* **Option A — Case-fold keyword matching** (chosen)
* **Option B — Reserve keyword names from being used as slugs**
* **Option C — Leave the asymmetry documented (status quo)**

## Decision Outcome

Chosen option: **Option A**, because the keyword corpus is already canonical lowercase — the admin
form validates `^[a-z][a-z0-9-]*$` — so folding the candidate in Go makes both lookups agree with
one line, needs no migration, and removes the per-driver difference instead of papering over it.
The keyword store is the single choke point (resolver host lookup, resolver path lookup, admin
duplicate check), so all three follow the same rule.

Option B leaves the routing question unanswered: even with reservation, an existing link named `gh`
is shadowed by a keyword registered later, so the ordering still needs one documented case rule.

### Consequences

* Good, because `/gh/x` and `/GH/x` (and `Host: GH`) reach the keyword template, so no case variant
  silently switches a request from the keyword admin owns to a user's link
* Good, because sqlite3, MySQL and PostgreSQL now resolve keyword paths identically
* Good, because the fix is in the store, mirroring how the slug path folds in the resolver: no
  collation, index, or migration change
* Neutral, resolution precedence is unchanged: keyword routing still runs before slug resolution,
  and a keyword name is still a creatable slug, shadowed by the keyword on every case variant
* Bad, because a keyword and a link sharing a name become indistinguishable by case — an operator
  who wanted case to tell them apart no longer can; nothing in the design ever allowed that, and
  the admin keyword list still shows the single canonical spelling

### Confirmation

Implementation is confirmed when:
1. With keyword `gh` and link `gh` both present, `/gh/joestump`, `/GH/joestump` and `/Gh/joestump`
   all return the keyword target
2. With keyword `go` registered and link `slack` present, `Host: GO` and `Host: go` both route via
   the keyword template
3. A path whose first segment is not a keyword still falls through to slug resolution, and the
   substituted remainder keeps its original case
4. Uppercase keyword creation is still rejected by the admin form, keeping the corpus canonical

## Pros and Cons of the Options

### Option A — Case-fold keyword matching

`GetByKeyword` lowercases its argument before the `WHERE keyword = ?` comparison.

* Good, because it matches the rule the resolver already applies to slugs
* Good, because the corpus is canonical lowercase by validation, so folding the argument is exact
* Good, because it is driver-independent and index-friendly (`LOWER()` on the column would not be)
* Neutral, because the admin duplicate check becomes case-insensitive as a side effect — desirable,
  and unreachable through the form, which rejects uppercase first

### Option B — Reserve keyword names as slugs

Add every registered keyword to the reserved-slug set at validation time.

* Good, because a user could never create a slug that a keyword shadows
* Bad, because `ValidateSlugFormat` is a pure static check; making it keyword-aware means a DB
  lookup on every create path (web, REST, MCP) and a migration to detect existing collisions
* Bad, because it does not answer the routing question for collisions created before the keyword,
  so Option A's rule is still needed
* Bad, because renaming or deleting a keyword would silently un-reserve a live slug

### Option C — Leave it documented (status quo)

* Good, because no behaviour changes
* Bad, because the security reviewer's finding in the #281 panel stays live: case selects the
  destination, and only on the sqlite3/PostgreSQL deployments most operators run

## Related

* Issue: stump.wtf/joe-links#284 (split out of the PR #281 review panel)
* ADR-0011 (Root Forward Keywords — the model this amends), ADR-0019 (case-folded slug resolution)
* SPEC-0019 REQ "Case-Insensitive Slug Resolution" (the recorded asymmetry this closes) and
  SPEC-0019 REQ "Case-Folded Keyword Matching" (the normative text this adds)
* SPEC-0008 REQ "Keyword Host Discovery" — the extension-side case-insensitive matching this aligns
  the server with
