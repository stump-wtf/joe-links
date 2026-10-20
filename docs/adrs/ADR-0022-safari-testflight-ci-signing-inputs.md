---
status: proposed
date: 2026-10-18
decision-makers: joestump-agent (proposed; needs joestump to confirm before #221 provisions the secrets)
extends: [ADR-0015]
governs: [SPEC-0015]
---

# ADR-0022: Inputs the TestFlight CI Job Signs With

## Context and Problem Statement

ADR-0015 chose TestFlight, and SPEC-0015 requires a `safari` job in
`.github/workflows/ci.yml` that archives the iOS and macOS Safari wrappers and
uploads them on every release tag. Issue #134 builds that job. The spec and the
SPEC-0015 design name most of what it needs, but leave three things open, and a
CI job cannot guess them:

* **The team ID.** SPEC-0015 says the project signs automatically "using an
  Apple Developer Program team", and the project file stores no
  `DEVELOPMENT_TEAM`. Every signed build needs one, so CI must supply it. The
  design's secret table has no entry for it.
* **Provisioning profiles.** The design lists `APPLE_PROVISIONING_PROFILE_IOS`
  and `APPLE_PROVISIONING_PROFILE_MACOS` as required. Automatic signing, given
  an App Store Connect API key, creates profiles on its own.
* **Prerelease tags.** This repo tags `v1.2.3-rc.1` as well as `v1.2.3`, and the
  spec's build number is made from the tag's digits, which ignores the suffix.

## Decision Drivers

* #221 is where the secrets get provisioned, so their names must be one list
  that CI and the operator agree on, in the open, before the first tag.
* Nothing committed may contain a credential, and the exported plists would have
  to embed the team ID if it were committed rather than passed at run time.
* A duplicate build number makes App Store Connect reject the *release* build,
  which is worse than skipping an rc upload.
* One signing path for both platforms, no fastlane, per the SPEC-0015 design.

## Considered Options

* **Team ID committed in the Xcode project.** Rejected: it is Joe's team, not a
  repository constant, and a fork with no team cannot release.
* **Team ID in a committed `ExportOptions.plist`.** Rejected: the same plist
  must be passed to `archive` too, and it commits half of the signing config
  while CI passes the rest.
* **Profiles required, as the design table says.** Rejected: with automatic
  signing plus the API key they are redundant, and requiring them makes the job
  fail on a first release that correctly omitted them.
* **Upload with fastlane `deliver`.** Rejected again here for the reason the
  design gives: a Ruby toolchain for one upload step.

## Decision Outcome

Chosen: pass the signing inputs to CI as secrets, and let automatic signing do
the rest.

* `APPLE_TEAM_ID` (name already used by the Safari guide) is required and is
  passed as `DEVELOPMENT_TEAM` to `archive` and as `teamID` in the generated
  `ExportOptions.plist`, which is therefore never committed.
* `APPLE_PROVISIONING_PROFILE_IOS` and `APPLE_PROVISIONING_PROFILE_MACOS` become
  optional: installed when set, otherwise created through the API key.
* Both archives override `MARKETING_VERSION` (the tag without `v`) and
  `CURRENT_PROJECT_VERSION` (the tag's digits, `v0.2.21` to `221`). The version
  matters as much as the number: App Store Connect only requires build numbers
  to be new within one version, and the project pins version `1.0`.
* Prerelease tags skip the job entirely rather than collide with the release.
* Upload stays on `xcrun altool`, and the step fails with an explicit message
  when a runner image no longer ships it, so the migration is a known task and
  not an unknown flag.

Required secrets, in the order the job checks them: `APPLE_CERT_P12`,
`APPLE_CERT_PASSWORD`, `APPLE_TEAM_ID`, `APP_STORE_CONNECT_API_KEY_ID`,
`APP_STORE_CONNECT_API_ISSUER_ID`, `APP_STORE_CONNECT_API_KEY_P8`.

### Consequences

* Good: one authoritative secret list, documented in the guide and checked by
  the job itself on every release run.
* Good: a fork with no Apple account gets a named missing secret, not a
  half-signed build.
* Bad: #221 must set `APPLE_TEAM_ID`, which its own description does not
  mention yet.
* Bad: the upload step breaks when Apple removes `altool`; the failure is loud
  and points at the replacement.

### Confirmation

First `v*.*.*` tag after #221 lands: both builds appear in App Store Connect
with the tag's version and derived build number.
