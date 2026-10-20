#!/usr/bin/env bash
#
# Prints the App Store Connect build number for a joe-links release tag.
#
# Governing: SPEC-0015 REQ "TestFlight Upload", scenario "Build number
# incremented on each upload" (v0.2.21 -> 221). App Store Connect rejects an
# upload whose build number is not new for that version stream, so the number
# must be derived deterministically from the tag rather than from the CI run
# number (a re-run would repeat it). Dots are removed and leading zeros
# dropped so v0.2.21 and v1.0.0 both yield the integer App Store Connect
# expects.
#
# Prerelease tags (v1.2.3-rc.1) are rejected on purpose: their digits collide
# with the final v1.2.3 build, and App Store Connect would then refuse the
# release build. The safari job skips those tags instead.

set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: $(basename "$0") <tag>" >&2
	exit 2
fi

tag="$1"

if [[ ! "$tag" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
	echo "error: '$tag' is not a release tag (expected vX.Y.Z, no prerelease suffix)" >&2
	exit 1
fi

number=$((10#${BASH_REMATCH[1]}${BASH_REMATCH[2]}${BASH_REMATCH[3]}))

if [ "$number" -eq 0 ]; then
	echo "error: tag '$tag' yields build number 0, which App Store Connect rejects" >&2
	exit 1
fi

echo "$number"
