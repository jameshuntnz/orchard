#!/usr/bin/env bash
#
# Build Orchard and install it on a Mac over ssh, supervised by launchd.
#
#   deploy/install.sh mini [remote-user]
#
# Safe to re-run: it replaces the binary and restarts the service, and never overwrites
# an existing orchard.env, so the token survives an upgrade.
#
# Needs the remote user's sudo password once per run, for /usr/local and
# /Library/LaunchDaemons. Everything the service itself does afterwards is unprivileged.

set -euo pipefail

host="${1:?usage: deploy/install.sh <ssh-host> [remote-user]}"
remote_user="${2:-admin}"
prefix=/usr/local/orchard

cd "$(dirname "$0")/.."

version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
echo "==> building orchard ${version} for darwin/arm64"
mkdir -p dist
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build \
	-trimpath -ldflags "-s -w -X main.version=${version}" \
	-o dist/orchard ./cmd/orchard

sed -e "s|__USER__|${remote_user}|g" -e "s|__PREFIX__|${prefix}|g" \
	deploy/net.orchard.plist >dist/net.orchard.plist

echo "==> copying to ${host}"
scp -q dist/orchard "${host}:/tmp/orchard.new"
scp -q dist/net.orchard.plist "${host}:/tmp/net.orchard.plist"

echo "==> installing (sudo password required on ${host})"
remote=$(cat <<REMOTE
set -euo pipefail

prefix=${prefix}
user=${remote_user}

install -d -o "\$user" -g staff -m 755 "\$prefix" "\$prefix/bin" "\$prefix/state"

# The token is generated on the host and never leaves it. An existing file is left
# alone, so re-running this does not invalidate whatever CI is already using.
if [ ! -f "\$prefix/orchard.env" ]; then
	umask 077
	cat >"\$prefix/orchard.env" <<ENV
ORCHARD_STATE_DIR=\$prefix/state
ORCHARD_TOKEN=\$(openssl rand -hex 32)
ORCHARD_HOSTNAME=orchard

# The CI upload listener: writes only, bearer token required, no path that serves a
# build or a page. Restrict it to the guest bridge subnets at the host firewall.
ORCHARD_UPLOAD_ADDR=0.0.0.0:8477

# ORCHARD_BASE_URL is left unset so it is derived from the tsnet name.
# TS_AUTHKEY is not set: on first run orchard prints a login URL to its log instead.
ENV
	chown "\$user" "\$prefix/orchard.env"
	chmod 600 "\$prefix/orchard.env"
	echo "    generated a new token in \$prefix/orchard.env"
else
	echo "    keeping the existing \$prefix/orchard.env"
fi

install -o "\$user" -g staff -m 755 /tmp/orchard.new "\$prefix/bin/orchard"
install -o root -g wheel -m 644 /tmp/net.orchard.plist /Library/LaunchDaemons/net.orchard.plist
rm -f /tmp/orchard.new /tmp/net.orchard.plist

launchctl bootout system/net.orchard 2>/dev/null || true
launchctl bootstrap system /Library/LaunchDaemons/net.orchard.plist
echo "    installed \$("\$prefix/bin/orchard" version)"
REMOTE
)

ssh -t "$host" "sudo bash -s" <<<"$remote"

cat <<NEXT

==> installed. Next:

  # watch it come up, and on first run copy the tailscale login URL out of the log
  ssh ${host} 'tail -f ${prefix}/orchard.log'

  # the bearer token for CI
  ssh ${host} 'grep ORCHARD_TOKEN ${prefix}/orchard.env'

NEXT
