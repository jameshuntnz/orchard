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
| `ORCHARD_MAX_UPLOAD_MB` | no | Upload cap (default 512) |
| `ORCHARD_MAX_BUILD_AGE_DAYS` | no | Age fallback; unset disables it |
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

## Installing on a Mac

```bash
deploy/install.sh mini
```

Builds a `darwin/arm64` binary, copies it over, and installs a launchd daemon that runs as
an ordinary user — nothing here needs root at run time, because tsnet's `:443` listener
lives inside the userspace netstack rather than on a host port. The token is generated on
the target into a `0600` env file and never leaves it. Re-running upgrades in place and
leaves the token alone.

## A note for testers

iOS identifies an installed app by its bundle identifier alone, so a test build carrying a
production bundle identifier lands in the **same app slot** as the copy already on the
phone — replacing it, and taking its local data with it. The install page shows the bundle
identifier prominently for that reason. Giving test builds their own identifier
(`com.example.app.adhoc`) avoids it, at the cost of a separate App ID and provisioning
profile.

## Development

```
go test ./... -race
```
