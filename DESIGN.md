# Orchard — Design

Internal build distribution over a tailnet. Builds ripen on the branch; you come and pick one.

**Status:** design, not yet implemented.

---

## 1. What this is

A small self-hosted service that receives ad hoc signed iOS builds from CI and serves them for over-the-air install to iPhones on the tailnet.

One build per branch per app, always the newest. A tester opens a URL on their phone and the app installs. Nothing is public, nothing goes near App Store Connect, and no third-party service is involved.

## 2. Why it exists

Getting an unmerged feature branch onto a tester's iPhone is harder than it looks. The obvious channel — TestFlight — cannot do it, for a reason worth recording because it rules out every variation of the idea.

App Store Connect enforces ordering on `CFBundleShortVersionString` at **upload** time. A build's marketing version must be greater than the app's last approved App Store version:

```
90062: The value for key CFBundleShortVersionString [0.0.2] in the Info.plist
       file must contain a higher version than that of the previously approved
       version [2.0.6]
```

That kills any scheme that marks test builds with a version *below* the real release train. The natural alternative — giving the test build the same marketing version as the branch it was cut from — fails too, wherever build numbers restart at `1` for each new release: App Store Connect requires the build number to *increase* within a marketing version, so an out-of-band build uploaded first will block the next release candidate.

The only remaining options inside TestFlight are a fake version band above the train, or abandoning per-release build numbering across the whole release process. Both pay a permanent cost to satisfy a store rule that has no bearing on an internal build.

Ad hoc distribution has no version rules at all. Nothing inspects the version, so a test build can say exactly what it is, and the release train is left completely alone.

A tailnet makes this practical. The CI host that already builds the IPA can serve it over HTTPS with a publicly trusted certificate — which iOS requires — without exposing anything to the internet.

## 3. Scope

**In scope**
- Host builds for **many apps**, each with many branches
- Receive an IPA plus metadata from CI, keyed by app and branch slug
- Generate the `itms-services` manifest iOS needs to install it
- Serve an install page per branch, an index per app, and a root index of apps
- Delete a branch's build on request; sweep builds whose branch is gone
- Bearer-token auth for writes; tailnet membership as the read boundary
- A versioned HTTP API that consumers can pin to

**Non-goals**
- Public distribution of any kind. No Funnel, no anonymous links.
- Replacing TestFlight for release candidates. Anything headed for the App Store keeps going through App Store Connect.
- Managing provisioning profiles, certificates or device registration. Orchard serves what CI hands it; Apple-side setup stays manual.
- History or rollback of builds. One build per branch, replaced in place.
- Orchestration. This is a single-node service on a tailnet, not a cluster workload.

## 4. Architecture

A single static Go binary that joins the tailnet as its own node and serves HTTP from a state directory on disk.

```
CI (macOS runner, any project)
  │  build + ad hoc sign
  │  POST /api/v1/apps/{app}/builds/{slug}   (multipart: ipa + metadata)
  ▼
Orchard  ──────────────────────────────────  tsnet node "orchard"
  │  writes  <state>/apps/<app>/builds/<slug>/{app.ipa,meta.json}
  │  renders manifest.plist + install page on read
  ▼
https://orchard.<tailnet>.ts.net/a/<app>/b/<slug>
  │  itms-services://?action=download-manifest&url=…
  ▼
Tester's iPhone (Tailscale app, device on the ad hoc profile)
```

### 4.1 Language and runtime — Go

- **A single static binary.** No runtime to install or patch on the host, no dependency resolution at deploy time, and no conflict with whatever else shares the box. Deployment is a file copy and a service restart.
- **`tsnet`.** Tailscale's Go library lets the process join the tailnet *itself*, as its own node with its own MagicDNS name and its own automatically provisioned certificate. No `tailscale serve` configuration to maintain, no reverse proxy, and the service's network identity is independent of the host's. This is the decisive constraint: `tsnet` is Go-only, and every alternative language means putting a proxy in front and managing certificates separately.
- The stdlib `net/http`, `html/template`, `archive/zip` and `embed` cover the entire feature set. No framework; the only meaningful third-party dependency is `tsnet` itself.

### 4.2 Tailnet integration

The service calls `tsnet.Server{Hostname: "orchard"}` and listens with `ListenTLS(":443")`. Tailscale provisions a Let's Encrypt certificate for `orchard.<tailnet>.ts.net` automatically.

The certificate matters more than it looks: iOS performs an `itms-services` install through a system daemon, and **it requires HTTPS with a publicly trusted certificate**. A self-signed cert on a LAN address will not work. `ts.net` certificates are ordinary publicly trusted certificates, which is what makes this whole approach viable.

> **Verified.** iOS's install daemon does route through the Tailscale tunnel. Tested against a `tailscale serve` endpoint on a tailnet node with an iPhone on iOS 26.6: the server logged both the manifest and the payload fetched by `com.apple.appstored/1.0`, iOS's install daemon, not by Safari. Serve was `(tailnet only)` — no Funnel involved.
>
> Note that Serve requires HTTPS certificates to be enabled for the tailnet, which publishes every certified machine name to public Certificate Transparency logs. The names become public; the machines do not become reachable.

### 4.3 Reaching Orchard from CI

A CI job generally **cannot** reach the tailnet, and on a well-configured build host it should not be able to. A host that runs untrusted-ish build jobs alongside deployment infrastructure will typically default-deny egress from job environments to private address space — including `100.64.0.0/10`, which is Tailscale's CGNAT range. A job that could reach the tailnet could reach every node on it, so this is a property worth keeping rather than an obstacle to route around.

What a job *can* reliably reach is **its own default gateway**, which is the host. That is the standard escape hatch for host-provided services on such a setup — package caches and registry mirrors are usually reached exactly this way.

So Orchard serves **two listeners**:

| Listener | Bind | Serves | Audience |
|---|---|---|---|
| Tailnet | `tsnet` `:443`, TLS via `ts.net` cert | Everything | Testers, humans |
| Upload | `ORCHARD_UPLOAD_ADDR`, plain HTTP | `/api/v1` **writes only** | CI jobs, via the host gateway |

The upload listener carries no browse pages and no IPA downloads — only the token-authenticated publish, delete and sweep endpoints. There is nothing to read through it, which keeps the blast radius small even though it is bound more broadly than the tailnet listener.

**Absolute URLs always come from `ORCHARD_BASE_URL`**, never from the request. A build published through the gateway listener still gets a manifest and install page addressed at the `ts.net` name, because that is where the phone will fetch them. This is what makes the split invisible: CI uses whichever route works for it, and the artefact it produces is identical either way.

**The gateway address is not fixed.** Host virtualisation typically allocates bridge subnets dynamically — `192.168.64.0/24`, then `.65`, and upward — and which guest lands on which varies across reboots and between VM and container providers. A job must therefore resolve its own default gateway at run time rather than having one baked into configuration. Linux reads `/proc/net/route`; macOS uses `route -n get default`.

The alternative — allowlisting Orchard's tailnet address through the host's egress filter — is possible where the filter supports it, but it is worse: it widens a deliberate security boundary, and MagicDNS does not resolve inside a job that isn't running Tailscale, so the job would need the raw CGNAT address while still presenting the `ts.net` hostname for TLS. The gateway route needs no security exception at all.


### 4.4 Implementation notes

Decisions an implementer would otherwise have to invent, recorded so two people building against this document agree.

- **Go 1.22 or newer**, for `net/http`'s method-and-wildcard routing patterns (`POST /api/v1/apps/{app}/builds/{slug}`). That is enough to serve every route above without a third-party router.
- **Module path** matches the repository. Layout: `cmd/orchard` for the binary and CLI, `internal/store` for the state directory, `internal/api` for the handlers, `internal/manifest` for plist and IPA inspection, `internal/web` for templates, `internal/update` for the self-updater.
- **HTTP timeouts are explicit on both listeners.** A default `http.Server` has no read or write timeout, which is a slow-loris waiting to happen. `ReadHeaderTimeout` should be short; the write and idle timeouts must be generous enough for a several-hundred-megabyte IPA to move over a tunnel, so the upload path needs a longer limit than the browse path.
- **Concurrent publishes to the same slug** are serialised with a per-slug lock. Both complete; the last to finish wins. Because publishing is write-to-temp-then-rename, a reader always sees one build or the other, never a mixture — but without the lock two renames can interleave and leave `app.ipa` and `meta.json` from different builds.
- **Logging is one structured line per request** — method, path, status, bytes, duration, and the tsnet peer where there is one — on stdout, for the supervisor to capture. No log files, no rotation to configure.
- **The state directory is created on start if absent**, with the tsnet subdirectory `0700`. Refuse to start if it exists and is not writable, rather than failing later on the first publish.

## 5. Apps

Every build belongs to an app. An app is an identifier, a display title, and a set of branch builds — nothing more.

- **Identifier** — `^[a-z0-9][a-z0-9-]*$`, chosen by the consumer and used in paths and URLs.
- **Created implicitly** on first publish. There is no registration step and no app-management API; an app with no builds does not exist.
- **Display title** comes from the most recent build's `title` metadata, so it stays current without a separate record to maintain.
- **Bundle identifiers are per build, not per app.** Two apps may legitimately share one, and one app's bundle ID may change over its life. Orchard never treats it as a key.

### 5.1 Installing replaces the tester's existing copy

iOS identifies an installed app by its bundle identifier alone. A test build carrying a production bundle identifier therefore lands in the **same app slot** as the copy the tester already has from the App Store or TestFlight — replacing it, and taking its local data with it. A failed install leaves the slot broken, and the tester has to delete the app by hand.

This is inherent to ad hoc distribution rather than anything Orchard does, and it is not obvious until it happens to someone. Two ways to handle it:

- **Accept it**, and say so plainly on the install page. Simplest, and fine when testers do not also need the production app working.
- **Give test builds their own bundle identifier** — `com.example.app.adhoc` alongside `com.example.app` — so a test build installs beside the real one instead of over it. This costs a separate App ID, its own provisioning profile, and matching changes to any app group, keychain group or associated domain the app relies on. In exchange the tester keeps a working production install, and the two are visibly distinct on the home screen.

The second is the better experience and the greater setup cost. It is the consumer's decision, not Orchard's — the service serves whatever bundle identifier it is handed. What Orchard *should* do is surface the identifier prominently on the install page, so a tester can see what is about to be replaced before tapping.

Apps are fully isolated in storage, in URLs, and in sweeps. A sweep for one app can never touch another's builds — worth stating explicitly because sweep is the one destructive operation exposed to callers.

## 6. HTTP API

All endpoints are versioned under `/api/v1`. Writes require `Authorization: Bearer <token>`; reads are unauthenticated, because tailnet membership is the boundary.

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

### Versioning policy

The path prefix is the version. Within `v1`, changes are additive only: new response fields and new optional request fields may appear, and consumers must ignore unknown fields. Anything that removes or repurposes a field, changes a status code, or alters a path ships as `/api/v2`, and `v1` keeps working for **at least one minor release** after `v2` lands, with a deprecation notice in `/healthz` and in release notes.

Browse paths (`/`, `/a/…`) are human surfaces and are not covered by this policy — they may change freely, since only a browser follows them.

`/healthz` returns what is running, so a consumer can assert compatibility rather than assume it:

```json
{
  "status": "ok",
  "version": "1.4.0",
  "apiVersions": ["v1"],
  "deprecated": []
}
```

### POST /api/v1/apps/{app}/builds/{slug}

`multipart/form-data` with two parts:

- `ipa` — the `.ipa`, `application/octet-stream`
- `metadata` — `application/json`:

```json
{
  "branch": "feature/new-checkout",
  "commit": "abcdef1234567890abcdef1234567890abcdef12",
  "version": "0.0.57",
  "buildNumber": "57",
  "bundleId": "com.example.app",
  "title": "Example",
  "notes": "TEST BUILD - do not release. Branch: feature/new-checkout, commit: abcdef1.",
  "runUrl": "https://ci.example.com/runs/123"
}
```

**`200`**

```json
{
  "url": "https://orchard.<tailnet>.ts.net/a/example/b/feature-new-checkout",
  "app": "example",
  "slug": "feature-new-checkout",
  "publishedAt": "2026-08-29T04:12:00Z"
}
```

Errors: `400` malformed metadata, bad app id or bad slug; `401` bad token; `409` the IPA's own bundle identifier disagrees with `bundleId`; `413` over the size cap.

Publishing is atomic — write to a temp directory, then rename into place — so a concurrent read never sees a half-written build.

### GET /api/v1/apps

```json
{
  "apps": [
    {
      "app": "example",
      "title": "Example",
      "builds": [
        {
          "slug": "feature-new-checkout",
          "branch": "feature/new-checkout",
          "commit": "abcdef1234567890abcdef1234567890abcdef12",
          "version": "0.0.57",
          "buildNumber": "57",
          "bundleId": "com.example.app",
          "publishedAt": "2026-08-29T04:12:00Z",
          "ipaSize": 41234567,
          "url": "https://orchard.<tailnet>.ts.net/a/example/b/feature-new-checkout"
        }
      ]
    }
  ]
}
```

Builds are newest first within an app; apps are ordered by their most recent build. This is the endpoint a consumer would use to reconcile state without scraping HTML.

### Error responses

Every non-2xx from `/api/v1` returns the same shape, so a caller can log something useful without special-casing each endpoint:

```json
{ "error": "invalid_slug", "message": "slug must match ^[a-z0-9][a-z0-9-]*$" }
```

`error` is a stable machine-readable code; `message` is for humans and may change freely. Codes in use: `invalid_slug`, `invalid_app`, `invalid_metadata`, `unauthorized`, `bundle_id_mismatch`, `payload_too_large`, `not_found`, `empty_keep`, `internal`.

### POST /api/v1/apps/{app}/sweep

```json
{ "keep": ["feature-new-checkout", "fix-timezone-drift"] }
```

Returns `{"removed": ["old-branch"], "kept": 2}`. An empty `keep` array is rejected: almost certainly a caller bug rather than a genuine instruction to delete every build in the app.

## 7. Storage

The filesystem is the database. No SQL, no embedded KV.

```
<state>/
  apps/
    example/
      builds/
        feature-new-checkout/
          app.ipa
          meta.json
        fix-timezone-drift/
          app.ipa
          meta.json
    another-app/
      builds/
        main/
          app.ipa
          meta.json
  tsnet/           tsnet's own state (node key, certs)
```

`meta.json` is the metadata from the upload plus `publishedAt`, `ipaSize` and a `schemaVersion`. Everything else — manifest, install page, indexes — is rendered on read from templates. Nothing derived is stored, so a template change takes effect without republishing anything, and an upgrade never has to rewrite existing content.

The total footprint is one IPA per active branch per app — tens of builds at most. Directory listing on every index render is fine at this scale and keeps the service stateless in memory.

**App ids and slugs are validated against `^[a-z0-9][a-z0-9-]*$` on every path that touches the filesystem.** They arrive over the network and become path segments; this is the only thing standing between the API and directory traversal.

## 8. The manifest

iOS reads a plist listing where the IPA is and what it claims to be. All URLs must be absolute, which is why the service needs to know its own external base URL.

```xml
<plist version="1.0"><dict><key>items</key><array><dict>
  <key>assets</key><array><dict>
    <key>kind</key><string>software-package</string>
    <key>url</key><string>https://orchard.…/a/<app>/b/<slug>/app.ipa</string>
  </dict></array>
  <key>metadata</key><dict>
    <key>bundle-identifier</key><string>com.example.app</string>
    <key>bundle-version</key><string>0.0.57</string>
    <key>kind</key><string>software</string>
    <key>title</key><string>Example</string>
  </dict>
</dict></array></dict></plist>
```

`bundle-identifier` must match the IPA's actual identifier or the install fails on the device with a message that does not explain why. Orchard therefore reads `Info.plist` out of the uploaded IPA at publish time and rejects a mismatch with `409`, turning a confusing device-side failure into a clear CI failure. This is the one piece of real validation the service performs and it earns its keep.

## 9. Web UI

Three pages, server-rendered, no JavaScript, no external assets. All should be legible in light and dark and readable on a phone, since that is where the install page is opened.

**Root (`/`)** — every app with at least one build: title, identifier, number of branches, most recent build time.

**App (`/a/{app}`)** — that app's branches, newest first: branch name, version and build number, short SHA, relative build time.

**Install (`/a/{app}/b/{slug}`)** — a "do not release" marker, app title and branch name, a large install button targeting the `itms-services` URL, and the build's metadata **including the bundle identifier**, since that is what determines which installed app this build is about to replace (§5.1). A **QR code for the page's own URL** is worth including: the tester needs to open this page *on the phone*, and the realistic alternative is copying a long `ts.net` URL out of a chat message. Rendering a QR as inline SVG is a small, dependency-light addition that removes the most annoying step in the flow.

## 10. Auth

**Writes** use a single bearer token, compared in constant time, supplied as `ORCHARD_TOKEN`. It grants write access to every app.

That is deliberate, and it is the main thing to revisit if Orchard ever hosts apps belonging to parties who should not be able to delete each other's builds. Per-app tokens would require apps to be registered explicitly rather than created on first publish, which is a real cost paid against a threat that does not exist on a single-team tailnet. Recorded here so the tradeoff is visible rather than accidental.

**Reads** are open to anyone on the tailnet. That is the intended boundary — the tailnet is the perimeter, and putting a second login in front of an internal build server would mostly make the tester's flow worse.

`tsnet` exposes the calling node's identity via `WhoIs`, so writes could later be restricted to specific nodes, or reads attributed to a person, without introducing a credential store.

## 11. Retention and cleanup

- **One build per branch per app.** A publish replaces that slug's directory. There is no history.
- **On branch close** — the consumer calls `DELETE /api/v1/apps/{app}/builds/{slug}`. A `404` is treated as success by convention, since most branches never get a build.
- **Periodic sweep** — the consumer lists its live branches, slugs them, and posts them as `keep` for its own app. Anything else in that app is removed. This catches branches deleted without a pull request, and anything a failed cleanup left behind.
- **Age fallback** — optionally drop builds older than N days regardless of branch state, as a backstop if a consumer stops calling in. Default off; if it runs at all it should log loudly.

Slugging is the consumer's responsibility, and a consumer's publish and cleanup paths must share one implementation or they will eventually disagree about which directory belongs to which branch. Orchard treats app ids and slugs as opaque identifiers and never derives one itself.

## 12. Configuration

Environment only; no config file.

| Variable | Required | Meaning |
|---|---|---|
| `ORCHARD_STATE_DIR` | yes | Where builds and tsnet state live |
| `ORCHARD_TOKEN` | yes | Bearer token for write endpoints |
| `ORCHARD_HOSTNAME` | no | tsnet node name (default `orchard`) |
| `ORCHARD_BASE_URL` | no | External base URL; derived from the tsnet name if unset |
| `ORCHARD_UPLOAD_ADDR` | no | Bind address for the CI upload listener, e.g. `0.0.0.0:8477`. Unset disables it, leaving the tailnet listener as the only route |
| `ORCHARD_MAX_UPLOAD_MB` | no | Upload cap (default 512) |
| `ORCHARD_MAX_BUILD_AGE_DAYS` | no | Age fallback; unset disables it |
| `ORCHARD_UPDATE_ENABLED` | no | Self-update on/off. Default `true`; forced `false` in a container (§13.2) |
| `ORCHARD_UPDATE_REPO` | no | Where releases are fetched from, `owner/repo`. Compiled-in default; overriding it changes what the service will execute, so treat it as a trust setting |
| `ORCHARD_UPDATE_CHANNEL` | no | Which releases are eligible (default `stable`; prereleases ignored) |
| `ORCHARD_UPDATE_INTERVAL` | no | How often to check (default `6h`) |
| `ORCHARD_IN_CONTAINER` | no | Set to `1` by the official image. Authoritative signal that this is a container; disables self-update (§13.2) |
| `TS_AUTHKEY` | first run | Tailscale auth key to join the tailnet |

## 13. Distribution — two supported forms

Orchard ships as a **static binary** and, optionally, as a **container image**. Both are first-class; they differ in how they are updated, and that difference is the reason the distinction matters at all.

### 13.1 Binary (default)

The primary form, and the one to reach for on a machine that also builds iOS apps — which means a Mac, where a container runtime would mean running a Linux VM to host a single static binary. It is also the only form that self-updates.

**Release artifacts per tagged version:**

```
orchard_<version>_darwin_arm64.tar.gz
orchard_<version>_linux_amd64.tar.gz
orchard_<version>_linux_arm64.tar.gz
SHA256SUMS
```

Run under launchd on macOS or systemd on Linux, as a dedicated non-admin user that owns the install directory and the state directory and nothing else. Owning its own install directory is what lets it replace its own binary without root — see §14.

### 13.2 Container image (opt-in, not published by default)

A `FROM scratch` image containing the same static binary. It costs one extra stage in the release pipeline and makes deployment on a container-only host a one-liner.

**Images are not published unless explicitly requested.** The release workflow takes a `publish_image` input defaulting to `false`; enabling it pushes to the registry alongside the tarballs. The default is off because an unpublished image cannot be accidentally depended upon, and because most deployments will be the binary.

Two things differ inside a container:

- **`tsnet` state must be a persistent volume.** The node key and provisioned certificates live in `ORCHARD_STATE_DIR`; if that is ephemeral the service re-registers as a brand-new tailnet node on every restart, accumulating dead nodes and losing its certificate. This is the single most likely container misconfiguration and the service should log a prominent warning on first run if it registers a new node in an environment it believes to be containerised.
- **Self-update is disabled.** The image tag is the unit of deployment; a process rewriting its own binary inside a container produces a running service that no longer matches its image, and the change vanishes on the next restart. Updating means pulling a new tag.

Container detection is explicit: the image sets `ORCHARD_IN_CONTAINER=1`, and the service treats that as authoritative. As a fallback it also probes for `/.dockerenv` and container markers in `/proc/1/cgroup`, so a hand-rolled image that forgets the variable still gets the right behaviour rather than a self-updating container.

## 14. Update strategy

Three things move independently and each needs its own answer.

### 14.1 The binary — self-updating

Running as a binary, Orchard updates itself. The alternative is a person with shell access on the host for every patch release, which in practice means updates do not happen.

**The mechanism**

1. On a timer (default every 6 hours, `ORCHARD_UPDATE_INTERVAL`) the service checks the configured release feed for a version newer than the running one.
2. It downloads the artifact for its own platform over HTTPS from the configured repository only.
3. It verifies the archive against the `SHA256SUMS` published alongside it. **A release without checksums is refused, not installed unverified.**
4. If a publish is in flight, the update defers to the next check rather than interrupting an upload mid-write.
5. It writes an update marker recording the currently running version, moves the current binary to `orchard.prev`, and installs the new one.
6. It drains in-flight requests and exits. The supervisor — launchd `KeepAlive`, systemd `Restart=always` — starts the new binary.

Exiting and letting the supervisor respawn is deliberate: it needs no privileges beyond writing to the install directory the service already owns, so there is no `sudo`, no root daemon, and no `launchctl` permission to arrange.

**Rollback is automatic.** On startup the new binary looks for the update marker. If one is present it runs a self-check — bind the listener, read the state directory, render one build page — and on failure restores `orchard.prev`, clears the marker, and exits so the supervisor brings the previous version back. On success it clears the marker and carries on. A crash loop in a bad release therefore self-corrects instead of needing someone to notice.

**What this trusts.** The service downloads a binary and executes it, so the download is the security boundary. Three things guard it: the release is fetched over HTTPS from the configured repository only, the archive is checked against published checksums, and an unchecksummed release is refused. What is *not* guarded: the checksums come from the same place as the archive, so this detects corruption and interrupted downloads, not a compromised repository. Signing the artifacts and verifying a pinned public key would close that, and is the obvious next step.

**Controls**

- `ORCHARD_UPDATE_ENABLED` — default `true` for the binary, forced `false` in a container regardless of the value.
- `ORCHARD_UPDATE_CHANNEL` — which releases are eligible, e.g. `stable`. Prereleases are ignored unless the channel says otherwise.
- `orchard update [--check] [--version X.Y.Z] [--rollback]` — the same logic on demand, for when waiting for the timer is not acceptable.

### 14.2 On-disk state

`meta.json` carries `schemaVersion`. The rules:

- A newer binary **must** read every older `schemaVersion` it has ever shipped with. Test fixtures for each retired version live in the repo permanently.
- Migration happens **lazily on read**, in memory. Nothing rewrites the state directory during an update, so an update cannot half-fail and leave the store inconsistent — and a rollback to the previous binary is always safe, because the previous binary still understands everything on disk.
- If a build cannot be read at all it is skipped and logged rather than crashing the index. One malformed directory should never take down the service.
- Because everything derived is rendered on read, a schema change never requires touching stored files.

That last property is what makes automatic rollback viable. If updates rewrote state on disk, rolling back the binary would mean rolling back the data too, and an automatic rollback would be far too dangerous to run unattended.

The safety net is that all state is disposable: the worst outcome is deleting the state directory and re-running each consumer's test build workflow once.

### 14.3 The API

Covered by the versioning policy in §6. The consumer pins a path prefix, `/healthz` reports what is served and what is deprecated, and a breaking change means a new prefix rather than a coordinated deploy.

This is why the API is versioned from the first release rather than retrofitted. Consumers live in other repositories with their own release cadences, and the service now updates itself on a timer — so there is no moment at which anyone chooses to deploy Orchard, and therefore no moment at which consumers could be updated in lockstep with it. Additive-only changes within `v1` are what make unattended self-update safe.

### 14.4 Ordering

Orchard and its consumers update independently and in any order. There is no deployment sequence to coordinate — which is the whole point of paying for versioning up front, and the precondition for letting the service update itself unattended.

## 15. Security

- **Path traversal** — every app id and slug validated against the pattern above before touching the filesystem. This is the highest-risk surface in the service.
- **Upload size** — capped via `http.MaxBytesReader`; an unbounded multipart read on a shared host is a trivial disk-fill.
- **Zip handling** — reading `Info.plist` out of an uploaded IPA means parsing an archive from an authenticated but not necessarily careful client. Bound the entry count and decompressed size; never write extracted entries to disk.
- **Token comparison** — `subtle.ConstantTimeCompare`; refuse to start if the token is unset or trivially short.
- **The upload listener is the widest surface.** It is plain HTTP bound beyond the tailnet, so: writes only, bearer token required on every route, no path that serves a build or a page, and the host firewall should restrict it to job subnets. Binding it at all is opt-in via `ORCHARD_UPLOAD_ADDR`.
- **No Funnel.** Public exposure is a one-flag mistake with real consequences, so the code should have no path that enables it, and this document should say why.
- **Defence in depth from Apple** — every IPA is ad hoc signed, so even a leaked build only installs on devices already registered on the profile.
- **Untrusted input in templates** — branch names, app titles and free-text notes arrive over the network and are rendered into HTML. `html/template` handles this correctly by default; the requirement is never to build page fragments by string concatenation.

## 16. Testing

- **Unit** — app id and slug validation (traversal, empty, unicode, over-length), manifest rendering, metadata parsing, `Info.plist` extraction, retention arithmetic, `schemaVersion` migration against fixtures for every retired version.
- **Integration** — publish, fetch the install page, fetch the manifest, delete, sweep, across two apps to prove isolation, against `httptest` with a temp state dir. The full lifecycle is small enough to test end to end in-process.
- **Upgrade** — start on version N-1's state layout, upgrade, assert every build still renders. Cheap, and the thing most likely to break silently.
- **Manual, once, before anything else** — install an ad hoc build on a real iPhone over the tailnet. Everything above assumes that works.

## 17. Open questions

1. **Do test builds get their own bundle identifier**, installing beside the production app, or do they replace it? See §5.1 — a consumer decision with real setup cost either way.
2. **Which port for the upload listener**, and does the host's egress filter need a rule to permit job subnets to reach it on the gateway? Resolved in principle by §4.3; the specific port and any host-side rule are deployment details.
3. **Does the host build Orchard itself**, or consume prebuilt release artifacts? Affects whether a toolchain is a deployment prerequisite.

## 18. Future work

- **Android APKs.** The same shape with less ceremony — an APK needs no manifest, just a download link, and no device registration at all. The app namespace already accommodates it; only the install page and manifest generation are platform-specific.
- **Per-app tokens**, if Orchard ever hosts apps across trust boundaries. See §10 for what it costs.
- **Per-viewer identity** via `WhoIs`, if it becomes useful to know who installed what.
- **Expiry notices** on the install page as a provisioning profile approaches its annual expiry, since an expired profile produces a device-side failure that is hard to diagnose.
