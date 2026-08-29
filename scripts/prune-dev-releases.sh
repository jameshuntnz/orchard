#!/bin/bash
#
# Vendored from jameshuntnz/sapling. The two projects release the same way on
# purpose — same channels, same version derivation, same commands — so this is a
# copy rather than a reimplementation. Fixes belong upstream first.
# Delete dev prereleases beyond the most recent few, with their tags.
#
# A dev build publishes for every green commit on main, at roughly 13.6MB of
# assets each. Left alone that is a few GB a month of builds nobody will ever
# install: a node updates forward, it does not reach back for dev.7.
#
# Only dev prereleases are considered. rc and stable releases are history and
# are never touched, whatever their age.
#
# Usage: prune-dev-releases.sh [keep]     (default 10)
#   Needs GITHUB_TOKEN and GITHUB_REPOSITORY.
set -uo pipefail

KEEP="${1:-10}"
REPO="${GITHUB_REPOSITORY:-}"
TOKEN="${GITHUB_TOKEN:-}"

if [ -z "$REPO" ] || [ -z "$TOKEN" ]; then
    echo "error: GITHUB_REPOSITORY and GITHUB_TOKEN must be set" >&2
    exit 1
fi

api() {
    curl -sS \
        -H "Authorization: Bearer $TOKEN" \
        -H "Accept: application/vnd.github+json" \
        -H "X-GitHub-Api-Version: 2022-11-28" "$@"
}

DOOMED="$(mktemp)"
trap 'rm -f "$DOOMED"' EXIT

api "https://api.github.com/repos/$REPO/releases?per_page=100" \
    | KEEP="$KEEP" python3 -c '
import json, os, sys

keep = int(os.environ["KEEP"])
try:
    releases = json.load(sys.stdin)
except json.JSONDecodeError:
    sys.exit("could not read the release list")
if not isinstance(releases, list):
    sys.exit(f"unexpected response: {releases}")

# Sorted rather than trusting the API to return newest first: deleting the
# wrong end of this list would remove the release a node is about to install.
dev = sorted(
    (r for r in releases
     if r.get("prerelease") and "-dev." in (r.get("tag_name") or "")),
    key=lambda r: r["created_at"],
    reverse=True,
)
for release in dev[keep:]:
    print(release["id"], release["tag_name"])
' > "$DOOMED"

if [ ! -s "$DOOMED" ]; then
    echo "nothing to prune"
    exit 0
fi

COUNT=0
while read -r ID TAG; do
    [ -n "${ID:-}" ] || continue
    echo "pruning $TAG"
    api -X DELETE "https://api.github.com/repos/$REPO/releases/$ID" > /dev/null
    # The tag outlives its release and would otherwise pile up on its own.
    # Safe to remove: version derivation reads the most recent tag, which is
    # by definition one being kept.
    api -X DELETE "https://api.github.com/repos/$REPO/git/refs/tags/$TAG" > /dev/null
    COUNT=$((COUNT + 1))
done < "$DOOMED"

echo "pruned $COUNT old dev release(s), kept the newest $KEEP"
