#!/bin/bash
#
# Vendored from jameshuntnz/sapling. The two projects release the same way on
# purpose — same channels, same version derivation, same commands — so this is a
# copy rather than a reimplementation. Fixes belong upstream first.
# Derive the next version for a release channel, from conventional commits.
#
#   next-version.sh dev      0.5.0-dev.7   every push to main
#   next-version.sh rc       0.5.0-rc.1    promoting a version toward release
#   next-version.sh stable   0.5.0         finished release
#   next-version.sh hotfix   0.4.1         patch on top of an existing release
#
# Prints nothing when no commit since the last release warrants one, which is
# how the release workflow decides whether to publish at all.
#
# Bump is taken from commit subjects since the last stable tag:
#   feat:                       minor
#   fix: / perf:                patch
#   feat!: / BREAKING CHANGE:   major (minor while 0.x — reaching 1.0 is a
#                                      decision, not an accident of wording)
#   docs/test/chore/refactor/ci/build/style:  no release
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

channel="${1:-stable}"
case "$channel" in
    dev | rc | stable | hotfix) ;;
    *) echo "usage: next-version.sh [dev|rc|stable|hotfix]" >&2; exit 2 ;;
esac

# Stable tags only: prereleases are staging posts, not the baseline a version
# is computed from. Sorted by version, not by date, so an out-of-order tag
# can't rewrite history.
last_stable="$(git tag --list 'v*' --sort=-v:refname \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)"

if [ -z "$last_stable" ]; then
    range=""
    current="0.0.0"
else
    range="${last_stable}..HEAD"
    current="${last_stable#v}"
fi

IFS=. read -r major minor patch <<< "$current"

# --- hotfix: a patch on the last release, regardless of what landed since ---
if [ "$channel" = "hotfix" ]; then
    [ -z "$last_stable" ] && { echo "no release to hotfix" >&2; exit 1; }
    echo "${major}.${minor}.$((patch + 1))"
    exit 0
fi

subjects="$(git log --format='%s' ${range:+"$range"})"
bodies="$(git log --format='%B' ${range:+"$range"})"
[ -z "$subjects" ] && exit 0

bump="none"
while IFS= read -r subject; do
    [ -z "$subject" ] && continue
    case "$subject" in
        *\!:*)              bump="major" ;;
        feat:*|feat\(*\):*) [ "$bump" = "major" ] || bump="minor" ;;
        fix:*|fix\(*\):*|perf:*|perf\(*\):*)
                            [ "$bump" = "none" ] && bump="patch" ;;
    esac
done <<< "$subjects"

grep -q '^BREAKING CHANGE:' <<< "$bodies" && bump="major"
[ "$bump" = "none" ] && exit 0

if [ "$major" -eq 0 ] && [ "$bump" = "major" ]; then
    bump="minor"
fi

case "$bump" in
    major) major=$((major + 1)); minor=0; patch=0 ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    patch) patch=$((patch + 1)) ;;
esac

base="${major}.${minor}.${patch}"

case "$channel" in
    stable)
        echo "$base"
        ;;
    dev)
        # Counts commits, not previous dev tags, so the number always moves
        # forward even when a dev build wasn't published for every commit.
        # HEAD when there's no baseline tag: an empty range counts nothing,
        # which would peg every dev build at dev.0 and stop nodes ever seeing
        # a newer one.
        count="$(git rev-list --count "${range:-HEAD}" 2>/dev/null || echo 0)"
        echo "${base}-dev.${count}+$(git rev-parse --short HEAD)"
        ;;
    rc)
        # Next in sequence for this base version.
        existing="$(git tag --list "v${base}-rc.*" | wc -l | tr -d ' ')"
        echo "${base}-rc.$((existing + 1))"
        ;;
esac
