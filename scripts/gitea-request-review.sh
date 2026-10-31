#!/usr/bin/env bash
# Ask Gitea to request a review on a pull request, then prove it attached.
#
# Gitea API 28.x registers review requests as
#   POST /repos/{owner}/{repo}/pulls/{index}/requested_reviewers
# with a JSON body {"reviewers":["<username>"]}. The older shape some lanes
# were told to use --
#   POST /repos/{owner}/{repo}/pulls/{index}/reviewers?reviewers=<username>
# -- is not a route on this build: it answers 405 with an empty body, and
# `tea api` exits 0 for any HTTP status, so the request looked like a success
# while no reviewer was ever attached (issue #336). Because a green exit code
# proves nothing here, this script always re-reads the pull request afterwards
# and fails when the reviewer is missing from `requested_reviewers`.
#
# Usage: gitea-request-review.sh [--host HOST] [--dry-run] <owner/repo> <index> <reviewer>...
set -euo pipefail

usage() {
	echo "usage: $(basename "$0") [--host HOST] [--dry-run] <owner/repo> <index> <reviewer>..." >&2
}

host=${GITEA_HOST:-gitea.stump.rocks}
dry_run=false
repo=""
index=""
reviewers=()

while [ $# -gt 0 ]; do
	case $1 in
	--host)
		[ $# -ge 2 ] || { echo "--host needs a value" >&2; exit 2; }
		host=$2
		shift 2
		;;
	--dry-run)
		dry_run=true
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	--)
		shift
		break
		;;
	-*)
		echo "unknown option: $1" >&2
		usage
		exit 2
		;;
	*)
		break
		;;
	esac
done

[ $# -ge 3 ] || {
	usage
	exit 2
}
repo=$1
index=$2
shift 2
reviewers=("$@")

case $repo in
*/*) ;;
*)
	echo "repo must be <owner>/<repo>, got: $repo" >&2
	exit 2
	;;
esac
# Both halves are interpolated into the request path, so they have to look like
# Gitea owner and repo names: a dot-dot or an extra segment would point the
# authenticated POST at some other endpoint entirely.
for part in "${repo%%/*}" "${repo#*/}"; do
	case $part in
	'' | . | .. | *[!A-Za-z0-9._-]*)
		echo "repo must be <owner>/<repo>, got: $repo" >&2
		exit 2
		;;
	esac
done
case $index in
'' | *[!0-9]*)
	echo "pull request index must be a positive integer, got: $index" >&2
	exit 2
	;;
esac

# Usernames are interpolated into JSON, so only accept the Gitea charset.
body_reviewers=""
for reviewer in "${reviewers[@]}"; do
	case $reviewer in
	'' | *[!A-Za-z0-9._-]*)
		echo "invalid reviewer name: $reviewer" >&2
		exit 2
		;;
	esac
	body_reviewers+="\"$reviewer\","
done
body="{\"reviewers\":[${body_reviewers%,}]}"

endpoint="repos/${repo}/pulls/${index}/requested_reviewers"

if $dry_run; then
	printf 'POST %s\n%s\n' "$endpoint" "$body"
	exit 0
fi

if [ ${#reviewers[@]} -eq 0 ]; then
	echo "no reviewer named" >&2
	exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
	echo "jq is required to verify that the reviewer attached" >&2
	exit 2
fi

# The request command is a seam: tests point it at a stubbed forge.
request_cmd=${GITEA_API_CMD:-tea api}
read -r -a request_argv <<<"$request_cmd"

post_status=$("${request_argv[@]}" --login "$host" -X POST -H 'Content-Type: application/json' -d "$body" "$endpoint" 2>&1) || true
if printf '%s' "$post_status" | grep -q '"message"'; then
	printf 'gitea answered: %s\n' "$post_status" >&2
fi

pr_json=$("${request_argv[@]}" --login "$host" "repos/${repo}/pulls/${index}")

failed=false
for reviewer in "${reviewers[@]}"; do
	if ! printf '%s' "$pr_json" |
		jq -e --arg user "$reviewer" '
        [.requested_reviewers[]?.login] | index($user) != null
      ' >/dev/null; then
		printf 'review request did not attach: %s is not in requested_reviewers on %s#%s\n' \
			"$reviewer" "$repo" "$index" >&2
		printf 'it now holds: %s\n' "$(printf '%s' "$pr_json" | jq -c '[.requested_reviewers[]?.login]')" >&2
		failed=true
	fi
done

if $failed; then
	exit 1
fi

printf 'requested review from %s on %s#%s (verified in requested_reviewers)\n' \
	"${reviewers[*]}" "$repo" "$index"
