---
status: proposed
date: 2026-11-05
decision-makers: proposed by joestump-agent, awaiting joestump
---

# ADR-0025: ADR Number Collisions Fail The Gate

## Context and Problem Statement

Both automation identities cut a branch off a moving `main` and pick the next ADR number that looks free there. Two lanes racing the same gap therefore pick the same number, and because the files have different paths the second merge is clean: `docs/adrs/ADR-0027-keyword-matching-case-folding.md` and `docs/adrs/ADR-0027-optional-oidc-local-admin.md` can both land, each `status: proposed`, and nothing anywhere notices. The failure surfaces later, in review, when "ratify ADR-0027" turns out to be a different question in each pull request, and every later reference to that number is ambiguous.

The same shape was found in a sibling repo at the same time (issue #345). This record decides where the check lives and what it refuses; it deliberately rules on nothing about those open pull requests.

## Decision Drivers

* **Caught by the gate, not by a reviewer.** A collision that only a human notices costs a review cycle on a question nobody meant to ask.
* **One implementation per repo.** The rule must not be rewritten in shell, in a workflow and in a test, because the three drift and one of them goes quiet.
* **Runs on the entry point lanes already use.** A lane runs `make test lint` before it pushes; a check wired anywhere else never runs.
* **No new dependency, no network** (ADR-0001). The gate runs offline, in a shallow checkout, and on a fork without forge credentials.
* **Deterministic.** A check that needs a forge token or a fetched base branch is green on one runner and red on another, and a red-until-green flip teaches people to re-run CI.

## Considered Options

* **A. Name records by issue number or date instead of a sequence.** No allocator, so nothing to race. Rejected as this step: it renumbers every existing record and every `Governing: ADR-NNNN` comment in the codebase, and it is a bigger decision than the gate that is missing.
* **B. A collision check in the repo's own gate.** One implementation, run by `make lint` and by the test suite, needing nothing but the working tree. Chosen.
* **C. Generalize the merge train's colliding-number check into a shared CI workflow every repo inherits.** The right eventual home for the pre-merge case, but it touches shared infrastructure another lane owns, and it cannot run from a lane's worktree. Kept for later, not done here.
* **D. Ask each lane to pick its number from the forge instead of from `main`.** That is a wider write-then-claim allocator with its own race, and it needs forge credentials inside every build. Rejected.

## Decision Outcome

Chosen: **B**. A build lane's own gate refuses a tree in which one ADR number is claimed by more than one record.

1. **One implementation**: `internal/adrguard` reads a directory of design records, treats `ADR-<digits>-<slug>.md` as a claim on that number (leading zeros stripped, so `ADR-24` and `ADR-0027` are one claim), and reports every number two or more files claim in the same directory. Files whose names carry no number claim nothing.
2. **Two entry points, one check**: `cmd/adr-check` is wired into `make lint`, and `internal/packaging` runs the same check over `docs/adrs/` under `go test ./...`. Neither duplicates the rule. Note where each actually runs: Actions on the canonical forge (gitea.stump.rocks) runs only gitleaks — branch pushes produce no other runs there — so the entry point that binds merge-bound work on the canonical host is a lane's own `make lint`; the test-suite entry point enforces wherever `go test ./...` runs, today only the GitHub push mirror, which does not gate merges here.
3. **The message names both paths**, and when the colliding records carry different titles it says so, because "one number, two decisions" needs a ruling rather than a renumber.
4. **Numbers are scoped per directory**, so a design record and a record of another kind may share a number.
5. **Allocating a number stays a judgement call** for the lane: a number beyond the highest one already claimed by an open pull request is the safe pick, and this record is itself an example — it took 0025 because 0024 was already claimed twice.

## Consequences

**Positive**

* A lane that adds a second record under a number already used in the tree is stopped by a failing gate that names both files, instead of by a reviewer.
* Wherever the check runs, a collision turns that run red, so the ambiguity cannot persist quietly — subject to the coverage limits below.
* No credentials, no network, no fetched base branch: the same result locally, in CI and on a fork.

**Negative**

* The check sees the tree it runs on. Two branches that are both still open, and neither of which contains the other's file, are not flagged until one of them lands — the pre-merge case belongs to the merge train's own colliding-number check (option C), and a lane should still check open pull requests before it allocates.
* The canonical forge does not run the check in CI: its Actions execute only gitleaks, and the `go test` job lives on the GitHub push mirror, which does not gate merges. Until option C lands on the canonical forge, nothing reruns the check after a merge, and the only enforcement for merge-bound work is a lane's own `make lint` / `make test`.
* A repo must opt in by pointing the check at its own records directory; it does not propagate on its own.

## More Information

* Issue #345 — two repos, four open pull requests, one number each; this change rules on none of them.
* `make lint` runs `adr-check`; `internal/adrguard` and `internal/packaging/adr_test.go` hold the rule and its tests.
