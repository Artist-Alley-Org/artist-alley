---
id: "0047"
title: Cross-platform packaging — Linux containers, macOS bundles, Windows MSI/service
status: accepted
date: 2026-06-06
area: ops
phases:
  - "1.50"
supersedes: []
related:
  - "0006"
  - "0008"
  - "0017"
  - "0034"
  - "0038"
tags:
  - ops
  - packaging
  - distribution
  - windows
  - macos
  - linux
excerpt: >-
  Artist Alley ships through three first-class platforms: Linux
  (Docker + apt/dnf + static binaries; current shipping channel),
  macOS (Homebrew + signed .app bundle + DMG, notarized), and
  Windows (signed MSI installer registering a Windows Service,
  built via WiX Toolset, with embedded Postgres for the turnkey
  SMB-studio path). Three personas drive the targeting: SRE /
  DevOps operator, engineer self-hoster, SMB studio admin.
---

> **Status note (2026-07-13):** distribution is **Docker-only until
> v1.0.0** (1.55.T-2 decision) — the canonical image is
> `ghcr.io/artist-alley-org/artist-alley` after the v0.1.0 org move.
> The three-platform end-state below stands as the v1.0.0 target.

## Context

Artist Alley's current distribution surface (per ADR 0006) targets
**one Go binary, multi-arch Docker images, `.deb` / `.rpm` packages
with systemd, static binaries, Homebrew formula**. That covers the
SRE / DevOps operator persona well — someone comfortable with Linux,
Docker Compose, and Postgres on their own infrastructure.

It does not cover **the other half of the actual audience**:

- **Small game studios** that want a turnkey install on a Win Server
  box and a familiar Windows-shaped admin surface.
- **Non-engineer archive teams** (museum / gallery / library /
  studio brand archives) where the IT environment is Windows-first
  and there is no Linux operator to call.
- **Solo / indie creators** running on a Mac, who want a `.app`
  they double-click and a Postgres they don't have to install.

These users will not stand up a Linux VM + Docker Compose. They want
a Windows installer that drops a service onto a box and stays there,
or a `.app` that just runs.

This ADR commits Artist Alley to **three first-class distribution
platforms** — Linux, macOS, Windows — and documents the strategy,
tooling, and persona mapping for each.

### Why Windows is no longer optional

The original "Windows: static binary on the Releases page" plan
covers the engineer-with-curl persona but not the **SMB studio
admin persona**. That admin:

- Doesn't have a Docker host.
- Doesn't install dependencies from the command line.
- Expects an MSI that walks them through setup with a wizard.
- Wants Artist Alley registered as a Windows Service so it survives
  reboots without their intervention.
- Wants the database "to just work" — not a separate Postgres
  install / configuration / port management exercise.

Without a real Windows installer, Artist Alley is structurally
inaccessible to a significant slice of its target market. Game and
animation studios increasingly run on Windows server infrastructure;
archive / museum / library back-office systems are
overwhelmingly Windows-first. The "self-hostable single binary"
promise has to *actually* be self-hostable on the operator's
platform of choice.

### Why macOS is also first-class (not just Homebrew)

Homebrew covers macOS engineers. It does not cover the **solo
creator / small-studio Mac admin** persona — someone who wants a
`.app` in Applications that double-clicks to run with no Terminal
involvement. A signed + notarized `.app` bundle (or a DMG that
installs one) is the equivalent of the Windows MSI for that
audience.

### Three target personas (the framing for every packaging decision)

| Persona | What they want | What platform | What ships |
|---|---|---|---|
| **SRE / DevOps operator** | `docker compose up`, observability, deploys via CI | Linux | Docker image, apt/dnf, static binary |
| **Engineer self-hoster** | `brew install`, run as foreground or systemd, manage Postgres themselves | macOS, Linux | Homebrew formula, static binary, systemd unit |
| **SMB studio admin** | Double-click installer wizard, system service, bundled DB | Windows, macOS | Signed MSI (Win Service), signed `.app` bundle (DMG) |

The current distribution covers the first two personas. This ADR
adds the third.

## Decision

Artist Alley ships through three first-class platforms with
distinct distribution channels per persona:

### Linux — current shipping channel (no change)

Distribution:
- **Docker images** — `ghcr.io/artist-alley-org/artist-alley` (mirrored to
  Docker Hub), multi-arch (amd64 + arm64), Sigstore-signed.
- **`.deb` and `.rpm` packages** with hardened systemd unit and
  `/etc/artist-alley/aa.env` config template. Postinstall walks
  the operator through next steps.
- **Static binaries** for `linux/{amd64, arm64}` on GitHub Releases
  with SHA256SUMS + SBOM.
- **Snap / Flatpak** — deferred until demand. Current channels
  cover both server and desktop Linux.

Default backend: external Postgres (operator-managed). Storage:
filesystem by default, S3-compatible optional.

### macOS — Homebrew + signed `.app` bundle

Two channels for two macOS personas:

- **Homebrew** (engineer self-hoster) — `brew install mscrnt/tap/artist-alley`.
  Installs the same static binary as Linux. `brew services start
  artist-alley` for the launchd-managed background service. Operator
  brings their own Postgres (typically via `brew install postgresql`).

- **Signed `.app` bundle in a DMG** (small-studio Mac admin) — a
  Mac-native double-click experience:
  - Universal binary (amd64 + arm64) inside `Artist Alley.app`.
  - Embedded Postgres (via `EmbeddedPostgres` Go library or
    bundled `postgres` binary in the `.app` resources) so the
    operator doesn't install a separate database.
  - LaunchAgent registration prompted on first run — survives
    reboot without admin intervention.
  - Apple Developer ID signature + notarization (mandatory for
    Gatekeeper-friendly distribution).
  - Auto-update via [Sparkle](https://sparkle-project.org/) (the
    de facto macOS app auto-updater).

### Windows — signed MSI with Windows Service + embedded Postgres

The headline new work. Three phases (Phase A / B / C below match
1.50.A / 1.50.B / 1.50.C in the roadmap).

**Phase A — Minimum viable MSI** (the SMB-studio-admin shipping path):

- Single `aa.exe` cross-compiled with **mingw-w64** (`GOOS=windows
  GOARCH=amd64 CGO_ENABLED=1`). cgo deps that ship native
  Windows builds get bundled (`libwebp.dll` for `chai2010/webp`,
  similar for libvips if needed).
- **WiX Toolset v4** for the MSI authoring. Industry standard,
  XML-heavy, supports everything we need (Service registration,
  custom actions, upgrade paths, conditional install).
- **Embedded Postgres** bundled in the install dir (via
  `EmbeddedPostgres` Go library or a vendored `postgres` binary).
  Operator doesn't install a separate database. Data dir under
  `%PROGRAMDATA%\ArtistAlley\db\`.
- **Windows Service registration** as `NT SERVICE\ArtistAlley`,
  start type Automatic, runs under a service account.
- Configuration UI: install wizard captures the bare minimum (port,
  initial admin account, storage location); the running service
  serves the existing web admin for everything else.
- **Authenticode code signature** — Artist Alley's signed binary
  is what installs; the MSI itself is also signed. Required for
  SmartScreen-friendly distribution. We acquire an OV or EV code-
  signing cert (~$200–$400 / yr); EV gets immediate SmartScreen
  reputation, OV builds reputation over time.
- **Core image support only at Phase A.** ffmpeg, ghostscript,
  ImageMagick, librsvg, poppler, unar are *not* bundled at Phase A;
  the running service detects their absence at boot, logs a
  friendly diagnostic, and disables the affected preview pipelines
  with a UI banner ("video previews disabled — install ffmpeg from
  https://ffmpeg.org/ and restart the service"). MSI ships at
  ~50–100 MB.

**Phase B — Full bundle** (the truly turnkey path):

- Same MSI shape, but the installer now bundles every native
  dependency: ffmpeg, ghostscript, ImageMagick, librsvg, poppler,
  unar (or equivalents). Each registered in the service's
  effective PATH (private to the install — does not pollute system
  PATH).
- Larger MSI (~500 MB – 1 GB).
- Auto-updater service runs as a separate scheduled task that
  checks for new MSI releases and prompts for installation. Pattern
  follows Chrome's Omaha-style background updater rather than
  blocking the running service.

**Phase C — MSIX for managed enterprise** (Intune / GPO / MS Store):

- MSIX bundle wrapping the same payload as the Phase B MSI.
- Distributable via Microsoft Store (enterprise channels), Intune,
  Group Policy, or Microsoft Endpoint Manager.
- Auto-update flows through the MS Store / Intune service rather
  than our updater.
- Targets the studio / studio-IT-with-MDM persona.

### Backend default per platform

| Platform / channel | Default DB | Notes |
|---|---|---|
| Linux Docker / .deb / .rpm | External Postgres | Operator-managed; current shipping default |
| Linux static binary | External Postgres | Operator-managed |
| macOS Homebrew | External Postgres | `brew install postgresql` recommended |
| macOS `.app` bundle | **Embedded Postgres** | Bundled in the `.app`; operator-invisible |
| Windows MSI (Phase A/B) | **Embedded Postgres** | Bundled; data dir under `%PROGRAMDATA%` |
| Windows MSIX (Phase C) | **Embedded Postgres** | Same |

The embedded-Postgres choice for the turnkey paths is deliberate.
SQLite was considered (zero-config, tiny dep), but Artist Alley's
features (sqlc-typed queries, pgvector for AI features, CHECK
constraints mirroring typed Go constants per ADR 0042, transactional
DDL for migrations) lean hard on Postgres-specific features.
Maintaining a SQLite-compatible code path would double our schema
surface forever for the convenience of two distribution channels.
Embedded Postgres ships ~80 MB of bundled binary and avoids the
divergence.

### Native dependency strategy

The "what runs alongside `aa.exe`" question per dependency:

| Dependency | Used for | Linux | macOS | Windows Phase A | Windows Phase B |
|---|---|---|---|---|---|
| `libwebp` | WebP encode / decode | OS pkg | `.app` resources | Bundled DLL | Bundled DLL |
| `libvips` (if adopted) | Fast image processing | OS pkg | `.app` resources | Bundled DLL | Bundled DLL |
| `ffmpeg` | Video transcode + thumbnails | OS pkg | `.app` resources | **Not bundled** (admin installs) | Bundled |
| `ghostscript` | PDF rasterization | OS pkg | `.app` resources | **Not bundled** | Bundled |
| `ImageMagick` | Format conversion fallback | OS pkg | `.app` resources | **Not bundled** | Bundled |
| `librsvg` | SVG rasterization | OS pkg | `.app` resources | **Not bundled** | Bundled |
| `poppler` | PDF parsing | OS pkg | `.app` resources | **Not bundled** | Bundled |
| `unar` / `7zip` | Archive extraction | OS pkg | `.app` resources | **Not bundled** | Bundled |
| `blender` | 3D thumbnail render | Separate container (per ADR 0034 add-on layer) | Same | Same | Same |

**The principle**: Phase A ships the smallest installable surface
that gets the operator running with image-only review workflow. The
operator-installs-ffmpeg-themselves diagnostic is friendly and clear.
Phase B is what we ship when we have the engineering capacity to
maintain the full bundle.

### Build pipeline additions

The release pipeline gains two new platform stages per tagged release:

1. **macOS stage** (`.github/workflows/macos-release.yml`):
   - GitHub-hosted macOS runner (or self-hosted if Apple Silicon is
     load-bearing for build performance).
   - Build universal binary (amd64 + arm64).
   - Sign + notarize `.app` bundle.
   - Produce `Artist Alley.dmg` for direct distribution.
   - Update Homebrew tap with the new release.

2. **Windows stage** (`.github/workflows/windows-release.yml`):
   - Self-hosted Linux runner with mingw-w64 cross-toolchain (cheaper
     + faster than GitHub-hosted Windows runners).
   - cgo cross-compile to `aa.exe` (amd64).
   - WiX Toolset on Linux (via the [`wixtools`](https://github.com/wixtoolset/wix4)
     dotnet CLI; runs on Linux containers).
   - Bundle embedded Postgres + libwebp + (Phase B) full native deps.
   - Authenticode sign the MSI + the embedded `aa.exe`.
   - Publish to GitHub Releases.

The existing Linux stage stays unchanged.

### Signing certificate posture

- **Windows**: Code-signing cert (OV at minimum, EV preferred for
  immediate SmartScreen reputation). Cert held in [HSM / hardware
  token]; CI signs via remote-signing API. Cost: ~$200–$400 / yr.
- **macOS**: Apple Developer ID (mandatory for notarization).
  $99 / yr. Signing key in a separate keychain on the macOS
  build runner.
- **Linux**: Sigstore + cosign (already in place for Docker images
  per ADR 0006). No per-platform cert acquisition needed.

Both Windows and macOS signing certificates are an operational
expense the project takes on permanently. Both are renewable
annually; expiry without renewal silently breaks every download on
the affected platform.

## Consequences

### Positive

- **Three first-class personas covered.** Linux SRE, engineer self-
  hoster, and SMB studio admin all have a real shipping path. The
  "self-hostable single binary" promise applies on every platform
  the user actually runs.
- **Windows opens the SMB studio + non-engineer archive team
  market.** This is a meaningful share of the actual target
  audience. Without a real Windows installer, that audience is
  structurally inaccessible.
- **macOS `.app` opens the solo-creator / Mac-admin market.**
  Smaller than the Windows opportunity but real — and the
  engineering investment is materially smaller because we already
  produce darwin binaries.
- **Embedded Postgres on the turnkey paths means zero-config
  install.** Operator double-clicks the installer; the service
  runs; the admin UI opens. No separate Postgres install,
  configuration, port-juggling. This is the "actually self-
  hostable" experience SMB operators need.
- **WiX Toolset is industry standard.** Long-haul tool with
  documentation, community knowledge, and continued Microsoft
  investment. Future contributors find help when they need it.
- **Sparkle for macOS auto-update is the de facto pattern.**
  Mac operators know it; we don't reinvent.
- **The phased dep-bundling policy lets us ship Windows Phase A
  fast.** ~50–100 MB MSI with image-only support is realistic in
  3–4 weeks. The full Phase B bundle is the follow-on.

### Negative

- **Real engineering investment.** Phase A alone (cgo cross-
  compile, embedded Postgres, WiX authoring, signing setup, install
  flow tested on Win 10/11/Server) is 3–4 weeks of work. Phase B
  + Phase C are follow-on phases of similar size.
- **Permanent ongoing maintenance.** Every new cgo dependency
  becomes a "does it cross-compile cleanly?" question. Every
  native-dep version bump becomes a Windows + macOS bundle update.
  Two more platforms to keep green in CI.
- **Signing-certificate operational expense and risk.** Cert in
  HSM / token; CI integration; renewal procedure; documented
  recovery if a cert is lost or compromised. New ongoing
  operational responsibility.
- **Embedded Postgres ships ~80 MB of bundled binary per
  platform.** The Phase A Windows MSI is 50–100 MB primarily
  because of this. Acceptable cost for the turnkey path; documented
  so it's not a surprise.
- **Phase A's "ffmpeg not bundled" diagnostic is a friction
  point.** Some operators will not install ffmpeg themselves and
  will think Artist Alley is broken. The friendly diagnostic +
  admin-UI banner is the mitigation; Phase B (full bundle)
  eliminates it.
- **Three platforms means three install runbooks.** Documentation
  surface grows. Guides for each persona have to stay current.
- **MSIX (Phase C) targets a niche.** Most SMB operators won't
  deploy through Intune / GPO. Worth shipping for enterprise
  studios with mature IT, but lowest priority.

## Alternatives considered

- **Stay container-first; tell Windows operators to install Docker
  Desktop.** Current shipping plan. Rejected because Docker Desktop
  is not the SMB studio's install path — it adds an entire layer of
  infrastructure the operator did not sign up for. Plus Docker
  Desktop's recent licensing changes for commercial users add a
  cost the operator wasn't anticipating.

- **Use go-msi instead of WiX.** Considered. Rejected because
  go-msi is functional but feature-thin (limited control over
  Service registration, custom actions, conditional install,
  upgrade behaviour). WiX's verbosity is real but its capability
  ceiling is far higher; for a long-lived shipping platform we want
  the ceiling.

- **Skip the MSI entirely; ship a portable .zip with `aa.exe` and
  an install script.** Considered. Rejected because the SMB-studio-
  admin persona explicitly wants the MSI experience — wizard,
  shortcuts, uninstall via Programs & Features, service registration
  out of the box. A portable zip is the engineer-self-hoster path,
  and we already ship the static binary on the Releases page for
  that.

- **SQLite backend behind a build tag for the truly turnkey
  path.** Considered. Rejected because (a) maintaining a SQLite-
  compatible schema doubles the database surface forever, (b) we
  lose pgvector + transactional DDL + CHECK-constraint mirroring,
  (c) embedded Postgres is only ~80 MB and operates with zero
  configuration — the turnkey win is preserved without the schema
  cost.

- **Ship only Homebrew on macOS; skip the `.app` bundle.**
  Considered. Rejected because Homebrew is the engineer path; the
  Mac-admin persona is real and well-served by Sparkle-updated
  `.app` distribution. The incremental engineering for the `.app`
  on top of the macOS binary we already produce is modest.

- **MS Store-only on Windows; skip the MSI.** Considered. Rejected
  because MS Store has friction for enterprise distribution (admin
  approval flows, store-listing process), and many SMB operators
  don't run Store-managed Windows builds. MSI is the lowest-
  friction first-party path.

- **Outsource Windows builds to a third-party packaging service.**
  Considered (e.g., Chocolatey distribution, third-party MSI
  builders). Rejected because the long-haul cost is similar to
  doing it ourselves, the operational dependency is permanent, and
  we lose control over the signing chain.

## Implementation

Phase 1.50 ships across three platform tracks. The Windows track
is the largest workstream; macOS and Linux are smaller additions.

- **1.50.A — Windows Phase A (minimum viable MSI).** cgo cross-
  compile via mingw-w64, WiX Toolset authoring, embedded Postgres
  bundling, Windows Service registration, Authenticode signing
  setup, install flow tested on Windows 10 / 11 / Server 2019 /
  Server 2022. Core image-only support; friendly diagnostics for
  missing optional deps (ffmpeg / ghostscript / etc). 3–4 weeks.

- **1.50.B — Windows Phase B (full bundle).** All native deps
  bundled in the MSI (ffmpeg, ghostscript, ImageMagick, librsvg,
  poppler, unar). Auto-updater service. Per-dep version-bump
  rotation. 3–4 weeks after 1.50.A.

- **1.50.C — Windows Phase C (MSIX for managed enterprise).** MSIX
  bundle from the Phase B payload. Intune / GPO documentation. MS
  Store listing (if desired). 2–3 weeks after 1.50.B.

- **1.50.D — macOS `.app` bundle + DMG.** Universal binary,
  embedded Postgres, LaunchAgent registration, Apple Developer ID
  signing + notarization, Sparkle auto-update. 2–3 weeks; can run
  parallel with Windows work.

- **1.50.E — Release pipeline additions.** GitHub Actions stages
  for the macOS and Windows builds. Signed artifacts on every tag.
  Pipeline runs to completion (Linux + macOS + Windows) before the
  release is announced. ~1 week of CI work; lands alongside 1.50.A.

Phase 1.50 work happens after the Phase 1.49 cleanup branch
completes (legacy fallback + migration baseline). Cross-compile
work intersects significantly with the migration policy from ADR
0046 (the baseline migration runs against embedded Postgres on
Windows + macOS turnkey installs, so the schema needs to be
canonical) — sequencing matters.

## References

- ADR 0006 — Go as target backend. Single-binary commitment that
  this packaging strategy realises across three platforms.
- ADR 0008 — Storage architecture. Filesystem-default backend
  works identically on all platforms; embedded data lives under
  the platform-appropriate location (`%PROGRAMDATA%\ArtistAlley\`,
  `~/Library/Application Support/Artist Alley/`).
- ADR 0017 — Monetization & licensing. The Ed25519 `.lic`
  validation runs identically on every platform; the installer
  doesn't change the licensing surface.
- ADR 0034 — Capability add-ons. Blender / AI add-ons remain
  out-of-band per-platform companion containers; the MSI / `.app`
  does not bundle them.
- ADR 0038 — Premium add-on layer. Premium add-ons ship as
  separate artifacts that the running Artist Alley service
  downloads and verifies; the installer doesn't change this either.
- ADR 0042 — Distributed catalogs. CHECK-constraint / typed-
  constant mirror invariant applies identically on Postgres
  regardless of platform.
- ADR 0046 — Migration baseline + squash policy. The baseline
  migration runs against embedded Postgres on the turnkey paths;
  the schema-as-source-of-truth invariant carries forward.
- [WiX Toolset v4](https://wixtoolset.org/) — Windows installer
  authoring.
- [WiX cross-platform CLI](https://github.com/wixtoolset/wix4) —
  the dotnet-based CLI that runs on Linux build runners.
- [Sparkle](https://sparkle-project.org/) — macOS auto-update
  framework.
- [EmbeddedPostgres for Go](https://github.com/fergusstrange/embedded-postgres) —
  bundled Postgres for the turnkey paths.
- [Apple Notarization workflow](https://developer.apple.com/documentation/security/notarizing_macos_software_before_distribution) —
  required for macOS Gatekeeper-friendly distribution.
