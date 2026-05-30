# ADR 0034: Capability add-ons — out-of-band heavy components

- Date: 2026-05-30
- Status: Accepted

## Context

The Go binary should stay **small and audit-able in an afternoon**. The
moment we bake CLIP weights (300 MB), Whisper models (200 MB → 3 GB
depending on flavour), Stable Diffusion / Flux / ComfyUI runtimes
(multi-GB), or Tesseract trained data (200 MB / language) into the
single-binary distribution, the core stops being lean — and we burden
every studio install with hardware demands their workflow doesn't ask
for. AAA studios picking the binary up for review-only use shouldn't
have to download 5 GB of ML weights they won't run.

ResourceSpace ships every feature as either in-tree or as a PHP plugin
under `plugins/`. The plugin pattern (`pluginname.yaml` manifest +
activate / deactivate / purge lifecycle, `include/plugin_functions.php`
:17) is the right shape for *behaviour*. It is the wrong shape for
*weights / containers / runtimes* — those are artefacts, not source.

Artist Alley already has the seed of the answer: the Blender worker
runs as a separate Docker container behind an opt-in compose profile
(`--profile workers`), brought up only by operators who need 3D
turntables. MinIO does the same with `--profile storage-s3`. The
**add-on pattern formalises this approach** so AI capabilities, OCR,
transcription, and embedding generators ship the same way: optional
companion containers / model files operators install from an admin
surface when they want the capability.

Home Assistant's architecture is the closest reference: HA Core stays
small, **Add-ons** (Docker containers from a registry) supply the
heavy lifting, **Integrations** are first-party in-process Python.
The split scales — HA has hundreds of add-ons without bloating Core.

## Decision

Add a **Capability add-on** layer to the architecture, sitting between
first-party integrations (in-process Go) and third-party plugins
(WASM via Extism, ADR future). Add-ons are:

- **First-party supported** — we curate the registry and stand behind
  the listings.
- **Out-of-band artefacts** — Docker images, model weights, runtime
  binaries — pulled from a registry on-demand.
- **Optional** — default install has zero add-ons running. Operators
  pick what they want from the **Capabilities** admin surface.
- **License-tier soft-gated** — *only* for cloud-bridged add-ons (we
  host the inference, customer pays via tier). Locally-run add-ons
  are open to all tiers regardless of hardware. **The operator's
  GPU is the operator's GPU.**

### Distinction from integrations and plugins

| | Integration | Add-on | Plugin |
|---|---|---|---|
| Who writes it | First-party | First-party | Third-party |
| Where it lives | Go package in core | Separate container / model artefact | WASM module |
| Distribution | In the binary | Pulled from `add-ons.artist-alley.org` (or operator-mirrored) on install | Pulled from a marketplace, signed by author |
| Heavy deps? | No | **Yes — the point** | Small (sandboxed) |
| Cardinality target | A specific named system (Slack, LDAP) | A capability slot (transcription, image embedding, AI tagging, OCR) | Long-tail, anything |
| Lifecycle | Always loaded | Install / start / stop / upgrade / uninstall via admin | Install / enable / disable per request |
| Sandbox | None — our code | OS-level (container) | WASM-level |

### Manifest format

A YAML manifest per add-on. Borrows from RS's `pluginname.yaml`
(`include/plugin_functions.php:108`) plus add-on-specific fields:

```yaml
# add-on: ai-clip
name: ai-clip
title: CLIP visual embeddings (ViT-L/14)
version: 1.0.0
author: artist-alley
maintainer_url: https://artist-alley.org/add-ons/ai-clip
icon: clip.svg
icon-colour: "#7c3aed"

# What capability slot this add-on fills. AA's provider interfaces
# (image-embedding, transcription, ocr, etc.) declare the slot names
# they consume; an add-on declares which slot it satisfies.
provides: image-embedding

# How the add-on is shipped.
artifact:
  kind: docker          # docker | model | bundle
  image: ghcr.io/mscrnt/aa-addon-clip:1.0.0
  digest: sha256:...    # required — installer verifies before run
  ports:
    - container: 8080
      protocol: http
  health:
    path: /health
    interval: 30s

# Operator-visible resource requirements. Surfaces in the admin UI
# BEFORE install so the operator can decide whether their box can
# host this. Not enforced — operators with smaller boxes get to
# install and find out.
resources:
  ram_mb: 2048
  disk_mb: 1200
  gpu:
    required: false       # CPU works, GPU faster
    preferred: nvidia-cuda
    vram_mb: 6144

# Whether AA hosts this for the operator (cloud-bridged, tier-gated)
# or the operator runs it locally (open to every tier). Locally-run
# add-ons never tier-gate — operators are free with their own iron.
hosting:
  modes: [local, cloud-bridge]
  default: local
  cloud-bridge:
    requires_tier: pro    # cloud-bridge mode only — local stays open
    metered: tokens       # how usage counts toward the license

# Per-capability config schema. Same shape as the existing
# system_config form generator — admin gets a typed form for free.
config:
  - key: model_size
    label: Model size
    type: enum
    options: [base, large, xlarge]
    default: large
  - key: batch_size
    label: Batch size
    type: int
    default: 8

# Licensing of the add-on artifact itself. Surfaces in the install
# dialog as a click-through.
license:
  spdx: Apache-2.0
  notice_url: https://github.com/mlfoundations/open_clip/blob/main/LICENSE

# Lifecycle hooks (optional). Run as one-shot job-queue jobs.
# Reuses the existing job queue from Phase 1.18.A.
hooks:
  pre_install: []
  post_install: [warmup]
  pre_uninstall: [drain]
  post_uninstall: [cleanup_cache]
```

### Registry

- **`add-ons.artist-alley.org`** — the curated first-party registry,
  served from Cloudflare Pages (the same infra as licensing /
  customer portal per ADR 0017). Manifest catalogue is plain JSON,
  signed with the same Ed25519 key family.
- **Operator mirror support** — air-gapped studios point at a local
  HTTPS mirror; the installer doesn't care.
- **Federation** is out of scope for add-ons — each instance manages
  its own. (Add-on STATE replication is intentionally not federated
  because hardware capability is inherently per-instance.)

### Admin surface

`/admin/capabilities`:

- **Catalogue** — browse registry, filter by capability slot, group
  by category.
- **Detail dialog** — manifest preview, resource requirements,
  license, hosting mode picker, config form (typed from manifest).
- **Install button** — pulls artefact, verifies digest against the
  signed manifest, starts the container (or stages the model file)
  via the existing Docker compose / job-queue plumbing. Progress
  streams in the UI.
- **Installed** — list of active add-ons with start / stop / upgrade
  / uninstall controls + resource usage (RAM / VRAM / disk) per
  add-on.
- **Logs** — last N lines streamed live (same primitive as Phase
  1.41 admin live-tail).

### Capability slot model

AA's existing provider interfaces (image-edit, chat, commerce,
ad-provider, etc. from ADRs 0021 / 0022 / 0030 / 0031) become formal
**capability slots**. New slots for the AI work:

- `image-embedding` — produces a vector for an image (CLIP-class).
- `text-embedding` — produces a vector for text.
- `transcription` — audio → text + timestamps (Whisper).
- `ocr` — image / PDF → text (Tesseract).
- `image-classification` — labels + confidence (CLIP / custom).
- `nsfw-classification` — labels-only specialised case.
- `image-generation` — prompt → image (Stable Diffusion, Flux,
  DALL-E).
- `image-inpaint` — mask + prompt → image (ADR 0026's existing
  provider interface).

A slot can have multiple add-ons installed; the operator picks one
as the active provider per slot.

### Provider precedence within a slot

1. **Locally-installed add-on** for the slot — chosen if installed
   AND hosting mode is `local`.
2. **Cloud-bridged add-on** for the slot — chosen if installed,
   hosting mode is `cloud-bridge`, AND license tier permits.
3. **External cloud provider** for the slot (OpenAI / Stability /
   etc. per ADR 0026) — chosen if configured.

The operator can override precedence per slot. The runtime calls
the first one that resolves.

### License-tier interaction (revised — no GPU gating)

Per the user's directive 2026-05-30: **AA does not gate-keep GPU**.
The operator's hardware is the operator's. Tier interaction
collapses to two cases:

| Hosting mode | Community | Pro | Enterprise |
|---|---|---|---|
| Local (operator runs it) | ✓ | ✓ | ✓ |
| Cloud-bridge (AA hosts) | — | ✓ (with token budget) | ✓ (unmetered for org plan) |

The `cloud-bridge.requires_tier` field in the manifest expresses this
constraint declaratively; the installer reads it. A Community-tier
operator who buys a GPU and runs CLIP locally is fully supported —
no nag, no upsell.

### Resource manifest is advisory, not enforcing

We surface RAM / VRAM / disk requirements before install but do NOT
block install on under-spec'd hardware. Operators are adults; some
will run a "needs 6 GB VRAM" model on 4 GB and accept the slowdown.
We log a warning and proceed.

### Versioning + upgrade

- Add-ons are versioned semver. Upgrade is a single click; the
  installer pulls the new image, runs `pre_uninstall` of the old,
  `pre_install` of the new, swaps, runs `post_install`.
- Pinning is supported via `version_pin` in the installed add-on
  state — operators who don't want auto-upgrade pin to a version.

### Audit + observability

- Every install / start / stop / upgrade / uninstall fires an
  `addon.*` event into the Phase 1.40 audit log.
- Add-on metrics (request rate, error rate, latency) surface in the
  Phase 1.41 Prometheus endpoint under an `addon_` prefix. The
  add-on declares which metrics it emits in the manifest;
  cardinality is bounded.

### Licensing of add-on artefacts

- Add-on artefacts ship under their own SPDX-identified licenses
  (Apache-2.0 for CLIP, MIT for Whisper, BSD for Tesseract, etc.).
- The AA-side glue code remains under the project's main license
  (AGPL + commercial per ADR 0016).
- "Mere aggregation" applies — we run add-ons as containers via
  subprocess / HTTP; we do not link into their address space.

## Consequences

**Positive**

- **Core binary stays under 50 MB.** Distribution speed + audit
  surface stay small.
- AI capabilities ship without forcing every operator to pay the
  storage cost.
- The pattern reuses the existing compose-profile + job-queue
  plumbing — no new orchestration layer.
- The license-tier story stays clean: per-tier add-ons are EXPLICITLY
  cloud-bridge-only; local hardware is operator-owned.
- Adding a new AI capability is a manifest + a container, not a
  binary rebuild.

**Negative**

- A small registry needs to exist before any add-on is useful.
  Mitigation: Phase 1.42 ships the registry + the first three
  add-ons (CLIP, Whisper, Tesseract) together so the surface is
  meaningful at v1.
- Manifest schema is a contract we have to evolve carefully.
  Mitigation: versioned manifest schema (`schema_version: 1`),
  legacy versions remain supported indefinitely.

## Alternatives considered

- **Bake everything into the binary.** Distribution bloat,
  audit-surface bloat, mandatory ML costs. Rejected.
- **Make every add-on a WASM plugin.** WASM doesn't host
  multi-GB ML models well; the runtime cost dwarfs any sandboxing
  benefit. Plugins remain the right answer for code-only
  extensions. Add-ons remain the answer for artefact-heavy
  capabilities.
- **Each add-on becomes a separate Docker compose service the
  operator wires manually.** What we have today for Blender +
  MinIO. Fine for a handful; doesn't scale. The Capabilities admin
  surface is the right level of abstraction once there are >5.
- **Tier-gate GPU access** — explicitly rejected per the
  user's instruction. Operators bring their own hardware; we don't
  charge for what we don't host.

## Reference

- Phase 1.42 in [`docs/roadmap.md`](../roadmap.md) — the registry
  + installer + initial add-ons.
- ADR 0017 — license-tier model (cloud-bridge tier interaction).
- ADR 0021 / 0022 / 0026 / 0030 / 0031 — existing provider
  abstractions that become formal capability slots.
- ADR 0033 — observability metrics surface under `addon_` prefix.
- RS `include/plugin_functions.php` — manifest + lifecycle
  reference pattern (in-process plugins, not heavy artefacts).
