#!/usr/bin/env bash
#
# Build Orchard for a remote host, copy it there, and run its own installer.
#
#   deploy/install.sh <ssh-host> [--user U] [--prefix P] [--upload-addr A]
#
# This script deliberately knows nothing about install paths, launchd, systemd or the
# environment file. All of that lives in `orchard install`, which is also what the
# self-updater will reuse — a second copy here would drift from it the first time
# either changed. This is only the part that cannot live in the binary: getting the
# binary onto a machine it is not yet on.
#
# On a host that already has the binary, skip this entirely:
#
#   sudo orchard install --user someone --upload-addr 0.0.0.0:8477
#   orchard doctor

set -euo pipefail

host="${1:?usage: deploy/install.sh <ssh-host> [--user U] [--prefix P] [--upload-addr A]}"
shift

cd "$(dirname "$0")/.."

# The installer's own flags are passed through untouched, so this script never has to
# learn about a new one.
args=("$@")

echo "==> inspecting ${host}"
read -r remote_os remote_arch <<<"$(ssh "$host" 'echo "$(uname -s) $(uname -m)"')"

case "$remote_os" in
Darwin) goos=darwin ;;
Linux) goos=linux ;;
*) echo "unsupported remote OS: $remote_os" >&2; exit 1 ;;
esac

case "$remote_arch" in
arm64 | aarch64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*) echo "unsupported remote architecture: $remote_arch" >&2; exit 1 ;;
esac

# A hand-installed binary gets the same dev version the release pipeline would derive
# for this commit, so it sorts correctly against what the release feed publishes. A
# commit hash would not be comparable at all, and self-update refuses to guess.
version=$(./scripts/next-version.sh dev 2>/dev/null || true)
ldflags="-s -w"
if [ -n "$version" ]; then
	ldflags="$ldflags -X main.stampedVersion=${version}"
else
	version="the version recorded in source"
fi

echo "==> building orchard ${version} for ${goos}/${goarch}"
mkdir -p dist
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
	-trimpath -ldflags "$ldflags" \
	-o "dist/orchard-${goos}-${goarch}" ./cmd/orchard

echo "==> copying to ${host}"
scp -q "dist/orchard-${goos}-${goarch}" "${host}:/tmp/orchard.install"
ssh "$host" 'chmod +x /tmp/orchard.install'

echo "==> running the installer (sudo password required on ${host})"
# The binary installs itself from wherever it is run, so the temporary copy is the
# installer and the installed copy is what it produces.
ssh -t "$host" "sudo /tmp/orchard.install install ${args[*]}; rm -f /tmp/orchard.install"
