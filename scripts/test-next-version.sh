#!/bin/bash
#
# Vendored from jameshuntnz/sapling. The two projects release the same way on
# purpose — same channels, same version derivation, same commands — so this is a
# copy rather than a reimplementation. Fixes belong upstream first.
# Verify next-version.sh against a throwaway repository.
#
# Version derivation decides what gets released and what a node will install,
# so it is worth testing — and it cannot be tested against this repository's
# own history without polluting it.
set -uo pipefail
FAILED=0
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(mktemp -d)"
mkdir -p "$REPO/scripts"
cp "${1:-$HERE/next-version.sh}" "$REPO/scripts/next-version.sh"
cd "$REPO"
git init -q; git config user.email t@t; git config user.name t
git add -A && git commit -qm "chore: init"

check() {
    local channel="$1" expected="$2"
    local got; got="$(./scripts/next-version.sh "$channel" 2>/dev/null || true)"
    if [ "$got" = "$expected" ]; then
        printf "  ok    %-7s -> %s\n" "$channel" "${got:-（none）}"
    else
        printf "  FAIL  %-7s expected %-20s got %s\n" "$channel" "$expected" "${got:-（none）}"
        FAILED=1
    fi
}
commit() { git commit -q --allow-empty -m "$1"; }

echo "--- nothing releasable ---"
commit "docs: words"; commit "chore: tidy"
check stable ""
check dev ""

echo "--- first feature ---"
commit "feat: first thing"
check stable "0.1.0"
check rc     "0.1.0-rc.1"
# dev carries commit count and sha, so match only the stable part
got="$(./scripts/next-version.sh dev)"
case "$got" in
    0.1.0-dev.*+*) printf "  ok    dev     -> %s\n" "$got" ;;
    *) printf "  FAIL  dev     expected 0.1.0-dev.N+sha got %s\n" "$got"; FAILED=1 ;;
esac

echo "--- dev counter advances with commits ---"
first="$(./scripts/next-version.sh dev)"
commit "fix: another change"
second="$(./scripts/next-version.sh dev)"
n1="${first#*-dev.}"; n1="${n1%%+*}"
n2="${second#*-dev.}"; n2="${n2%%+*}"
if [ "$n2" -gt "$n1" ]; then
    printf "  ok    dev     -> %s then %s\n" "$first" "$second"
else
    printf "  FAIL  dev counter did not advance: %s then %s\n" "$first" "$second"
    FAILED=1
fi

echo "--- rc sequence increments ---"
git tag v0.1.0-rc.1
check rc "0.1.0-rc.2"
git tag v0.1.0-rc.2
check rc "0.1.0-rc.3"

echo "--- releasing stable resets the baseline ---"
git tag v0.1.0
check stable ""
commit "fix: a bug"
check stable "0.1.1"

echo "--- hotfix ignores what landed since ---"
commit "feat: unrelated work in progress"
check hotfix "0.1.1"

echo "--- breaking change while 0.x bumps minor, not to 1.0 ---"
git tag v0.2.0
commit "feat!: breaking"
check stable "0.3.0"

echo "--- past 1.0, breaking bumps major ---"
git tag v1.0.0
commit "refactor: restructure

BREAKING CHANGE: moved everything"
check stable "2.0.0"

echo "--- prerelease tags do not become the baseline ---"
git tag v2.0.0
git tag v2.1.0-rc.1
commit "fix: another"
check stable "2.0.1"

cd / && rm -rf "$REPO"
[ "$FAILED" = "0" ] && echo "version derivation ok" || exit 1
