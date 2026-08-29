#!/usr/bin/env bash
#
# Publish, delete and sweep Orchard builds from CI.
#
# A CI job generally cannot reach the tailnet, and on a well-configured build host it
# should not be able to: a job that could reach the tailnet could reach every node on it.
# What a job can reliably reach is its own default gateway, which is the host. So this
# script resolves the gateway at run time and talks to Orchard's upload listener there
# (DESIGN §4.3).
#
# Host virtualisation allocates bridge subnets dynamically — 192.168.64.0/24, then .65,
# and upward — and which guest lands on which varies across reboots and between VM and
# container providers, so nothing about the address is baked in.
#
# Set ORCHARD_URL to bypass gateway resolution entirely (for a runner that is itself on
# the tailnet).
#
# Usage:
#   orchard-publish.sh publish --app APP --branch BRANCH --ipa PATH \
#       --version V --build N --bundle-id ID --title T [--commit SHA] [--notes TEXT] [--run-url URL]
#   orchard-publish.sh delete  --app APP --branch BRANCH
#   orchard-publish.sh sweep   --app APP --branch BRANCH [--branch BRANCH ...]
#
# Environment:
#   ORCHARD_TOKEN  required, the bearer token
#   ORCHARD_URL    optional, e.g. https://orchard.your-tailnet.ts.net
#   ORCHARD_PORT   optional, upload listener port (default 8477)

set -euo pipefail

port="${ORCHARD_PORT:-8477}"

die() { echo "orchard-publish: $*" >&2; exit 1; }

# --------------------------------------------------------------------- addressing

default_gateway() {
	case "$(uname -s)" in
	Linux)
		local hex
		hex=$(awk '$2 == "00000000" && $8 == "00000000" { print $3; exit }' /proc/net/route)
		[ -n "$hex" ] || die "no default route in /proc/net/route"
		# The address is little-endian hex, so the octets come out back to front.
		printf '%d.%d.%d.%d\n' \
			"0x${hex:6:2}" "0x${hex:4:2}" "0x${hex:2:2}" "0x${hex:0:2}"
		;;
	Darwin)
		route -n get default 2>/dev/null | awk '/gateway:/ { print $2; exit }'
		;;
	*)
		die "unsupported platform $(uname -s)"
		;;
	esac
}

base_url() {
	if [ -n "${ORCHARD_URL:-}" ]; then
		printf '%s' "${ORCHARD_URL%/}"
		return
	fi
	local gw
	gw=$(default_gateway)
	[ -n "$gw" ] || die "could not resolve the default gateway"
	printf 'http://%s:%s' "$gw" "$port"
}

# --------------------------------------------------------------------- slugging

# Orchard treats app ids and slugs as opaque and never derives one itself, so slugging is
# the consumer's job. Publish and cleanup share this one implementation deliberately: two
# copies eventually disagree about which directory belongs to which branch (DESIGN §11).
slugify() {
	printf '%s' "$1" |
		tr '[:upper:]' '[:lower:]' |
		sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//' |
		cut -c1-100 |
		sed -E 's/-+$//'
}

json_escape() {
	printf '%s' "$1" |
		sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/\t/\\t/g' |
		awk 'NR > 1 { printf "\\n" } { printf "%s", $0 }'
}

# --------------------------------------------------------------------- arguments

cmd="${1:-}"
[ -n "$cmd" ] || die "expected a command: publish, delete or sweep"
shift

app=""; ipa=""; version=""; build=""; bundle_id=""; title=""
commit=""; notes=""; run_url=""
branches=()

while [ $# -gt 0 ]; do
	case "$1" in
	--app)       app="$2"; shift 2 ;;
	--branch)    branches+=("$2"); shift 2 ;;
	--ipa)       ipa="$2"; shift 2 ;;
	--version)   version="$2"; shift 2 ;;
	--build)     build="$2"; shift 2 ;;
	--bundle-id) bundle_id="$2"; shift 2 ;;
	--title)     title="$2"; shift 2 ;;
	--commit)    commit="$2"; shift 2 ;;
	--notes)     notes="$2"; shift 2 ;;
	--run-url)   run_url="$2"; shift 2 ;;
	*)           die "unknown option $1" ;;
	esac
done

[ -n "${ORCHARD_TOKEN:-}" ] || die "ORCHARD_TOKEN is not set"
[ -n "$app" ] || die "--app is required"
[ ${#branches[@]} -gt 0 ] || die "--branch is required"

url=$(base_url)
auth="Authorization: Bearer ${ORCHARD_TOKEN}"

case "$cmd" in
publish)
	[ ${#branches[@]} -eq 1 ] || die "publish takes exactly one --branch"
	[ -n "$ipa" ] || die "--ipa is required"
	[ -f "$ipa" ] || die "no such file: $ipa"
	[ -n "$version" ] || die "--version is required"
	[ -n "$bundle_id" ] || die "--bundle-id is required"
	[ -n "$title" ] || die "--title is required"

	branch="${branches[0]}"
	slug=$(slugify "$branch")
	[ -n "$slug" ] || die "branch '$branch' slugs to an empty string"

	# The metadata goes in a file rather than inline: curl's -F treats an unescaped
	# semicolon in the value as the start of a parameter, which silently truncates any
	# notes containing one.
	meta=$(mktemp)
	trap 'rm -f "$meta"' EXIT
	cat >"$meta" <<-JSON
		{
		  "branch": "$(json_escape "$branch")",
		  "commit": "$(json_escape "$commit")",
		  "version": "$(json_escape "$version")",
		  "buildNumber": "$(json_escape "$build")",
		  "bundleId": "$(json_escape "$bundle_id")",
		  "title": "$(json_escape "$title")",
		  "notes": "$(json_escape "$notes")",
		  "runUrl": "$(json_escape "$run_url")"
		}
	JSON

	echo "orchard-publish: ${app}/${slug} -> ${url}" >&2
	curl --fail-with-body --silent --show-error \
		--connect-timeout 10 --max-time 1800 \
		-H "$auth" \
		-F "ipa=@${ipa};type=application/octet-stream" \
		-F "metadata=@${meta};type=application/json" \
		"${url}/api/v1/apps/${app}/builds/${slug}"
	echo
	;;

delete)
	[ ${#branches[@]} -eq 1 ] || die "delete takes exactly one --branch"
	slug=$(slugify "${branches[0]}")

	# A 404 is treated as success by convention: most branches never get a build
	# (DESIGN §11).
	code=$(curl --silent --show-error --connect-timeout 10 -o /dev/null -w '%{http_code}' \
		-X DELETE -H "$auth" "${url}/api/v1/apps/${app}/builds/${slug}")
	case "$code" in
	200|404) echo "orchard-publish: ${app}/${slug} deleted (${code})" >&2 ;;
	*)       die "delete failed with status ${code}" ;;
	esac
	;;

sweep)
	keep=""
	for branch in "${branches[@]}"; do
		slug=$(slugify "$branch")
		[ -n "$slug" ] || continue
		[ -z "$keep" ] || keep="${keep},"
		keep="${keep}\"${slug}\""
	done
	# An empty keep is rejected by the server: almost certainly a caller bug rather than
	# an instruction to delete every build in the app.
	[ -n "$keep" ] || die "no live branches resolved to slugs; refusing to sweep"

	curl --fail-with-body --silent --show-error --connect-timeout 10 \
		-X POST -H "$auth" -H 'Content-Type: application/json' \
		-d "{\"keep\":[${keep}]}" \
		"${url}/api/v1/apps/${app}/sweep"
	echo
	;;

*)
	die "unknown command '$cmd' (want: publish, delete, sweep)"
	;;
esac
