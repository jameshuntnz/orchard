# Orchard

Internal build distribution over a tailnet. Builds ripen on the branch; you come and pick one.

A small self-hosted service that receives ad hoc signed iOS builds from CI and serves them
for over-the-air install to iPhones on the tailnet. One build per branch per app, always
the newest. Nothing is public and no third-party service is involved.

See [DESIGN.md](DESIGN.md) for why this exists and what it deliberately does not do.

## Running it

```
ORCHARD_STATE_DIR=/usr/local/orchard/state \
ORCHARD_TOKEN=<32+ random bytes, hex> \
orchard serve
```

The process joins the tailnet as its own node — separate from the host's — and Tailscale
provisions a publicly trusted certificate for `orchard.<tailnet>.ts.net`. iOS's install
daemon requires exactly that; a self-signed certificate on a LAN address will not work.

On first run, with no `TS_AUTHKEY` set, the log carries a Tailscale login URL. Open it once
and the node key persists in the state directory.

For a look at the pages without joining a tailnet:

```
orchard serve --dev-addr 127.0.0.1:8477
```

### Configuration

Environment only; no config file.

| Variable | Required | Meaning |
|---|---|---|
| `ORCHARD_STATE_DIR` | yes | Where builds and tsnet state live |
| `ORCHARD_TOKEN` | yes | Bearer token for write endpoints; at least 16 characters |
| `ORCHARD_HOSTNAME` | no | tsnet node name (default `orchard`) |
| `ORCHARD_BASE_URL` | no | External base URL; derived from the tsnet name if unset |
| `ORCHARD_UPLOAD_ADDR` | no | Bind address for the CI upload listener, e.g. `0.0.0.0:8477`. Unset disables it |
| `ORCHARD_UPLOAD_ALLOW` | with the listener | CIDRs allowed to use it, or `any`. Required whenever the listener is bound |
| `ORCHARD_MAX_UPLOAD_MB` | no | Upload cap (default 512) |
| `ORCHARD_MAX_BUILD_AGE_DAYS` | no | Age fallback; unset disables it |
| `ORCHARD_UPDATE_ENABLED` | no | Self-update on/off (default `true`; always off in a container) |
| `ORCHARD_UPDATE_REPO` | no | Where releases come from, `owner/repo`. Overriding it changes what the service will execute |
| `ORCHARD_UPDATE_CHANNEL` | no | Which releases are eligible (default `stable`; prereleases ignored) |
| `ORCHARD_UPDATE_INTERVAL` | no | How often to check (default `6h`) |
| `ORCHARD_UPDATE_TOKEN` | for a private repo | Credential for the release API |
| `ORCHARD_IN_CONTAINER` | no | Set by the official image; disables self-update |
| `TS_AUTHKEY` | no | Tailscale auth key, for unattended first run |

## Two listeners

A CI job generally cannot reach the tailnet, and on a well-configured build host it should
not be able to. What a job *can* reach is its own default gateway, which is the host. So
Orchard serves two listeners:

| Listener | Bind | Serves | Audience |
|---|---|---|---|
| Tailnet | tsnet `:443`, TLS via the `ts.net` cert | Everything | Testers, humans |
| Upload | `ORCHARD_UPLOAD_ADDR`, plain HTTP | `/api/v1` **writes only** | CI jobs, via the host gateway |

Absolute URLs always come from `ORCHARD_BASE_URL`, never from the request, so a build
published through the gateway still gets a manifest addressed at the `ts.net` name — which
is where the phone will fetch it. CI uses whichever route works for it and the artefact is
identical either way.

### Restricting the upload listener

The upload listener is plain HTTP, so its bearer token crosses the wire in cleartext.
Bound to `0.0.0.0` it answers to every network the host is on — including the LAN, and the
tailnet it was specifically not meant to serve.

Binding it to the guest bridge instead does not work: virtualisation creates and destroys
those interfaces with the guests, so the address is usually absent when the service starts.
The restriction is therefore a source allowlist, and it is **required** whenever the
listener is bound:

```
ORCHARD_UPLOAD_ADDR=0.0.0.0:8477
ORCHARD_UPLOAD_ALLOW=127.0.0.1/32,192.168.64.0/18,172.16.0.0/12
```

A source outside it is refused before routing or token comparison, with the same response
a bad token gets — telling an unexpected caller that it was refused for *being* unexpected
hands it the one fact it did not have. The detail goes to the log instead.

`any` disables the check. It has to be written explicitly, because an unrestricted
plain-HTTP write listener should be a decision rather than an omission.

For defence in depth on macOS, `orchard firewall` prints packet-filter rules for the same
port and the same CIDRs, and `sudo orchard firewall --apply` installs them as a pf anchor:

```bash
sudo orchard firewall --apply
```

The service already refuses these callers; this stops the port answering them at all. It
is a separate command rather than part of `install` because pf is shared with anything
else on the host that uses it, and reloading it should not be a side effect of putting a
binary in place. The ruleset is parsed with `pfctl -n` before it is loaded, the previous
`/etc/pf.conf` is kept as `.orchard.bak`, and the rules reference no interface and no
other service — a renumbered bridge or somebody else's anchor cannot change what they do.

Keep both layers: the pf rules are absent whenever pf is disabled, and that is not
something the service can detect.

## API

All endpoints are versioned under `/api/v1`. Writes require `Authorization: Bearer <token>`;
reads are unauthenticated, because tailnet membership is the boundary.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/api/v1/apps/{app}/builds/{slug}` | bearer | Publish, replacing any existing build for that app and slug |
| `DELETE` | `/api/v1/apps/{app}/builds/{slug}` | bearer | Remove one branch's build |
| `POST` | `/api/v1/apps/{app}/sweep` | bearer | Within this app only, remove every build whose slug is absent from `keep` |
| `GET` | `/api/v1/apps` | — | Machine-readable list of apps and their builds |
| `GET` | `/` | — | Index of apps |
| `GET` | `/a/{app}` | — | Index of that app's branch builds |
| `GET` | `/a/{app}/b/{slug}` | — | Install page |
| `GET` | `/a/{app}/b/{slug}/manifest.plist` | — | `itms-services` manifest |
| `GET` | `/a/{app}/b/{slug}/app.ipa` | — | The build |
| `GET` | `/healthz` | — | Liveness, service version, API versions served |

App ids and slugs must match `^[a-z0-9][a-z0-9-]*$`. Every non-2xx from `/api/v1` returns
`{"error": "<stable code>", "message": "<for humans>"}`.

Within `v1`, changes are additive only, and consumers must ignore unknown fields. Anything
that removes or repurposes a field, changes a status code, or alters a path ships as
`/api/v2`.

## Publishing from CI

`scripts/orchard-publish.sh` resolves the guest's own default gateway at run time — bridge
subnets are allocated dynamically, so nothing about the address can be baked in — and shares
one slugging implementation across publish, delete and sweep. Two copies of that logic
eventually disagree about which directory belongs to which branch.

```bash
export ORCHARD_TOKEN=…

scripts/orchard-publish.sh publish \
  --app example --branch "$GIT_BRANCH" --ipa build/Example.ipa \
  --version 0.0.57 --build 57 \
  --bundle-id com.example.app.adhoc --title Example \
  --commit "$GIT_SHA" --run-url "$CI_RUN_URL"

# on branch close
scripts/orchard-publish.sh delete --app example --branch "$GIT_BRANCH"

# periodically, from the list of live branches
scripts/orchard-publish.sh sweep --app example --branch main --branch feature/x
```

Set `ORCHARD_URL` to skip gateway resolution, for a runner that is itself on the tailnet.

## Installing

The binary installs itself. On the host:

```bash
sudo orchard install --user someone --upload-addr 0.0.0.0:8477
```

It creates `/usr/local/orchard/{bin,state}` owned by the service user, generates a
`0600` environment file with a fresh token, installs itself into place, and registers a
supervisor — **launchd on macOS, systemd on Linux**. Nothing needs root at *run* time,
because tsnet's `:443` listener lives inside the userspace netstack rather than on a host
port; root is needed only to write to `/usr/local` and register the service.

Every step is check-then-fix, so it is safe to re-run: it fixes only what is missing,
upgrades the binary in place, and never overwrites an existing environment file — the
token CI is using survives an upgrade.

`--prefix` moves the install root. Installing into a prefix you already own needs no sudo
at all, apart from registering the supervisor.

To see the same checks without changing anything:

```bash
orchard doctor
```

`doctor` runs exactly the checks `install` acts on, so it cannot drift into a second
opinion. It exits non-zero when something needs attention. A check that cannot run reports
as unverified rather than green — reporting green on an unknown is worse than reporting
nothing.

### From another machine

`deploy/install.sh` is a thin convenience wrapper for a host that does not have the binary
yet. It detects the remote OS and architecture, cross-compiles, copies the result over, and
runs `orchard install` there, passing your flags through untouched:

```bash
deploy/install.sh mini --upload-addr 0.0.0.0:8477
```

It deliberately knows nothing about install paths or service definitions — that lives in
the binary, which is also what the self-updater will reuse.

## A note for testers

iOS identifies an installed app by its bundle identifier alone, so a test build carrying a
production bundle identifier lands in the **same app slot** as the copy already on the
phone — replacing it, and taking its local data with it. The install page shows the bundle
identifier prominently for that reason. Giving test builds their own identifier
(`com.example.app.adhoc`) avoids it, at the cost of a separate App ID and provisioning
profile.

## Updating itself

Running as a binary, Orchard updates itself. The alternative is a person with shell access
for every patch release, which in practice means updates do not happen.

On a timer it checks the release feed, downloads the artifact for its own platform,
verifies it against the `SHA256SUMS` published alongside, writes a marker, moves the
current binary to `orchard.prev`, installs the new one, drains and exits. The supervisor
starts what is now in place. That last step is the whole mechanism: it needs no privilege
beyond writing to the install directory the service already owns, so there is no `sudo`,
no root daemon and no `launchctl` permission to arrange.

**Rollback is automatic.** On startup the new binary finds the marker and runs a
self-check — bind a listener, read the state directory, render a page. On failure it
restores `orchard.prev`, keeps the failed binary as `orchard.failed` for evidence, and
exits so the supervisor brings the previous version back. A crash loop in a bad release
therefore corrects itself. This is only safe because nothing rewrites state on disk during
an update, so the previous binary still understands everything it left behind.

A publish in flight defers the update to the next check rather than interrupting a
transfer that may have taken minutes.

```bash
orchard update --check              # what is available
orchard update                      # fetch, verify, install
orchard update --version 1.2.0      # a specific release, older ones included
orchard update --rollback           # put the previous binary back
```

**What this trusts.** The service downloads something and then executes it, so the
download is the security boundary. Three things guard it: the release is fetched over
HTTPS from the configured repository only, the archive is checked against checksums
published alongside it, and **a release without checksums is refused rather than installed
unverified**. What is *not* guarded: the checksums come from the same place as the archive,
so this detects corruption and interrupted downloads, not a compromised repository.
Signing the artifacts and verifying a pinned public key would close that, and is the
obvious next step.

Releases are cut by tagging:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

## What Orchard assumes about its host

One thing only: **that the host is its guests' default gateway.** That is true of any VM
or container host, and `ORCHARD_URL` in the publish script bypasses even that for a runner
already on the tailnet. Orchard has no knowledge of any particular CI system, and needs
none — it serves what it is handed, over a route the job resolves for itself.

The reverse also holds: nothing about a CI host needs to know Orchard exists. A host that
default-denies egress to private address space already permits a job to reach its own
gateway, because DHCP, DNS and package caches depend on that; Orchard just uses a port on
a path that was already open.

## Development

```
go test ./... -race
```

CI runs on a self-hosted [Sapling](https://github.com/jameshuntnz/sapling) node, in a
Linux container built from `.sapling/images/go/Dockerfile` — the bare runner has no C
compiler, and `go test -race` links the race runtime through cgo. That is a dependency of
this *repository's* pipeline, not of the service: `go test ./... -race` and
`go build ./cmd/orchard` work anywhere with a Go toolchain.
