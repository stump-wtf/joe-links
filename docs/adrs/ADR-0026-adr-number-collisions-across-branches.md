---
status: proposed
date: 2026-11-07
decision-makers: proposed by joestump-agent, awaiting joestump
---

# ADR-0026: ADR Number Collisions Across Branches Fail CI

## Context and Problem Statement

ADR-0025 put the collision check inside the tree it runs on, and recorded the limit that came with it: "Two branches that are both still open, and neither of which contains the other's file, are not flagged until one of them lands." That is not an edge case — it is the report issue #345 was filed from. `ADR-0024-keyword-matching-case-folding.md` (PR #335) and `ADR-0024-optional-oidc-local-admin.md` (PR #343) were both open at the same time, each branch holding one of them, each tree looking clean on its own, and neither gate said anything. The ambiguity therefore still arrives: it just arrives at the second merge instead of at review.

The same ADR-0025 note says the check must not need "a fetched base branch", because a check that is green on one runner and red on another teaches people to re-run CI. This record keeps that driver in mind and decides where the base comparison belongs, rather than pretending the branch-only view is enough.

## Decision Drivers

* **The failure must be visible before review, not at the second merge.** A lane that lands a collision has already cost a review cycle by the time the tree disagrees with itself.
* **One implementation of the rule.** The cross-branch check widens what the existing check can see; it does not restate what a collision is. A second implementation in shell or YAML is how a gate goes quiet.
* **A merge, not a union.** The set worth checking is what the merge would leave behind. Treating both tips as a plain union would fail a branch whose only change is a legitimate renumber — it deleted the old path — which is the fix the gate itself tells a lane to make.
* **Deterministic where it runs, optional where it cannot.** `make lint` stays runnable offline and credential-free; the base comparison runs where the base is actually present.

## Considered Options

* **A. Leave the branch-only check as it is and rely on the merge train's colliding-number check (ADR-0025 option C).** Right eventual home, wrong scope for this step: it is shared infrastructure another lane owns, and it cannot run from a lane's worktree.
* **B. Put the base comparison in `make lint` too.** Every lane would run it, but a lane in a shallow clone, a fork without `origin/main`, or offline would go red for a reason unrelated to its own work — the green-then-red flip ADR-0025 rules out.
* **C. Compare this branch against a fetched base, in CI, using the merge result as the visible set.** Chosen: it needs nothing but git history, adds no implementation of the rule, and fails where the collision would otherwise land silently.
* **D. Have the check ask the forge which ADR files open pull requests add.** Widest coverage, but it needs forge credentials in CI, cannot run from a lane's worktree, and makes the gate's verdict depend on another pull request's timing.

## Decision Outcome

Chosen: **C**.

1. **`adr-check -base <ref>` compares the merge, not the branch.** The visible set is everything on either side, minus what the branch deleted since the two diverged. `internal/adrguard.MergedPaths` holds that set logic; `internal/adrguard` still owns what a collision is, so the rule stays in one place and a design record and its renumber cannot both be reported.
2. **The rule is unchanged.** A number is still claimed by `ADR-<digits>-<slug>.md`, leading zeros still carry no identity, and numbers are still scoped per directory. What widened is only which records the check can see.
3. **A failure says which side each record is on.** `docs/adrs/ADR-0024-optional-oidc-local-admin.md (on origin/main)` is the difference between rebasing and renumbering, so the message names the tree as well as the path.
4. **`make adr-vs-base` is a separate target, not part of `make lint`.** CI runs it on every pull request and on `main`. Locally it is one command, and when the base ref is absent it fails with the fetch that would make the comparison possible rather than reading as a pass — a comparison that saw nothing is not a comparison that passed.
5. **Renumbering stays a legal move.** A branch that deletes its own colliding record and re-adds it under a free number produces, after the merge, one record per number, and the gate passes it.

## Consequences

**Positive**

* A pull request cut after this gate is on `main` goes red on its own CI run when the merge would collide with a record `main` already holds, naming the file on the other side, instead of waiting for whichever lands second. Branches cut before the gate landed run neither the target nor the workflow, so the four open pull requests issue #345 lists stay outside its reach.
* Nothing new runs on a lane's machine unless the lane asks for it, so ADR-0025's offline and no-credentials properties are untouched.
* The gate is now enforced on the canonical forge's own pull requests, which is where merges are gated and where ADR-0025 noted only gitleaks ran.

**Negative**

* The CI job needs the base branch's history, so it fetches with `fetch-depth: 0`. That is slower than a shallow checkout and is a reason to fold this into the shared workflow once that exists (ADR-0025 option C).
* Two branches that never merge — a PR closed unmerged, a branch abandoned — are still not compared. The gate describes merges.
* A record added on the base and deleted on the branch after the divergence point resolves as a deletion, matching the merge. Where that is a conflict rather than a deletion, git says so first and the gate is not the thing to blame.

## More Information

* ADR-0025 — the tree-local check this widens, and the coverage limit it recorded.
* Issue #345 — two repos, four open pull requests, one number each.
* `internal/adrguard` holds the rule and the visible-set logic; `cmd/adr-check` holds the git plumbing; `Makefile` exposes both entry points; `.gitea/workflows/adr-gate.yml` runs the comparison on the canonical forge.
