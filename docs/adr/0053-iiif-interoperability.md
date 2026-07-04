---
id: "0053"
title: IIIF Image API + Presentation API for institutional interoperability
status: accepted
date: 2026-06-26
area: extensibility
phases:
  - "1.54"
supersedes: []
related:
  - "0034"
  - "0043"
tags:
  - extensibility
  - standards
  - interop
  - glam
  - iiif
excerpt: >-
  Expose every image asset through the IIIF Image API 3.0 and every collection through the IIIF Presentation API 3.0, so cultural-heritage operators can install AA as a Mirador / Universal Viewer-compatible catalogue without custom builds.
---

## Status (2026-07-03)

Both sub-phases shipped. IIIF arc complete.

- **Phase 1.54.A** (PR #165, 2026-06-25): IIIF Image API 3.0 Level 0 over the existing variant pipeline. Manifest endpoints (`info.json`), region / size / rotation / format / quality parameters, content-hash-keyed tile cache, capability gate (`iiif.read`), `/admin/iiif/health` per the generic subsystem-health pattern.
- **Phase 1.54.B** (PR #187, 2026-07-03): IIIF Presentation API 3.0 collection + asset manifests at `/iiif/3/{kind}/{id}/manifest.json` with navPlace geo-tag extension for GPS-tagged assets and embargo-stub manifests per ADR 0020; Content Search 2.0 at `/iiif/3/{kind}/{id}/search` (asset-scope substring-scans metadata pairs; collection-scope dispatches through the 1.16.B `search.Engine` filtered to pinned members); 2.0→3.0 URL redirect at `/iiif/2/...` (301, `full`→`max` size grammar); federated canvas resolver (read-only `federation_peers.instance_url` lookup with 5-min in-process cache, lives at `app/internal/iiif/federation/` NOT `app/internal/federation/` — soak diff empty); `iiif.ManifestCache` via `cache.Registry` + LISTEN/NOTIFY with cross-package invalidator hooks on asset + collection write paths; dashboard subsystem card on `/admin/search/dashboard`. Issue #170 closed.

### 1.54.B carry-forward follow-ups (filed as separate issues)

- **1.54.C** (PR #194, 2026-07-03): SHIPPED. `/iiif/3` external URL alias via Go dual-mount at root — all four IIIF handlers register at both `/api/v1/iiif/...` and `/iiif/...`. Original brief called for nginx rewrite; reality forced dual-mount because `ui-pr.yml` CI uses the prod `embed_web` app image standalone (no nginx) — same shape as any operator running the docker image without a reverse proxy. Issue #188 closed.
- **1.54.D** (PR TBD, 2026-07-03): SHIPPED. Automated Mirador dogfood via Playwright on self-hosted nightly (`ui-13-iiif-mirador-dogfood.spec.ts`). Structural DOM asserts on canvas thumbnails / metadata sidebar / Content Search 2.0 hits with retained screenshot artifacts (30d) for operator post-hoc review. Mirador loaded via unpkg.com CDN, nightly-only until 30-day flake data justifies PR-CI promotion. Vite dev config extended to proxy `/iiif` alongside `/api` (nightly hits Vite; 1.54.C's dual-mount is behind it). Manual operator dogfood step from #193 fully automated. Issue #193 closes on three consecutive green nights.
- **1.54.E** — Per-page PDF tile routing (multi-page PDFs currently surface as a single canvas with a `Pages: N` metadata pair; per-page canvases wait on Image API `/iiif/3/{id}/pages/{n}/...` URL grammar)
- **1.54.F** — Content Search asset-scope per-line text extraction (currently substring-scans metadata pairs; real granularity needs `asset_text` FTS table populated by the metadata pipeline)
- **1.54.G** — Custom `provider` block sysconfig UI (currently uses derived defaults)
- **1.54.H** — Content Search `AnnotationCollection` pagination when hit count ≥ 50

### 1.54.B design decisions locked

- **Anonymous callers gated at the IIIF layer** rather than modifying the shared `visibility.Filter` (which would have rippled through 1.16.B-1..B-4 test expectations). Consolidation of the two paths tracked at #185.
- **Federation resolver lives OUTSIDE `app/internal/federation/`** at `app/internal/iiif/federation/` per soak rule. Empty-string fallback keeps a broken peer directory from blocking manifest render (degraded remote canvas, not 500).
- **No IIIF Auth API in v1** — anonymous-first per 1.54.A extended.
- **No IIIF Annotation write-back** — read-only Content Search 2.0 only.
- **Mirador + OpenSeadragon interop asserted structurally** (canvas count, thumbnail render, tile paint). No pixel snapshots — brittle across Mirador upgrades. Automated by 1.54.D: Mirador manifest-render + Content-Search-in-viewer + metadata-sidebar assertions run on the self-hosted nightly (`scripts/dogfood/ui/tests/standalone/ui-13-iiif-mirador-dogfood.spec.ts`) with screenshot artifacts retained 30 days on `.pw-results/` for operator post-hoc review (~2 min per merge, down from ~30 min manual dogfood). Manual operator dogfood step from #193 fully automated 2026-07-03.

## Context

RS ships full IIIF support (`iiif_functions.php`); AA shipped nothing until 1.54.A. The RS-gap audit (2026-06-22) flagged IIIF as the strategic differentiator for the GLAM (Galleries / Libraries / Archives / Museums) market: a museum operator who installs AA and immediately gets a Mirador-compatible catalogue gets institutional credibility AA cannot otherwise claim. The cost of being IIIF-compliant is far lower than the cost of being IIIF-noncompliant in that market.

IIIF is also a forcing function for clean asset-canvas semantics that benefit non-GLAM users — the Presentation API's canvas / manifest / range vocabulary maps cleanly onto AA's existing post / collection / asset model and gives third-party tooling (slide-deck builders, exhibit planners, scholarly annotation tools) a standard surface to integrate against.

### Why a separate ADR from the existing API design

The IIIF endpoints are NOT a typed extension of the REST API — they have their own URL shape (`/iiif/3/...`), their own response semantics (JSON-LD, not pure JSON), their own auth conventions (the IIIF Auth API spec; not AA's API-token shape), and their own caching characteristics (content-hash-addressable forever). Documenting them as an architectural concern separately from the operator-facing REST surface keeps the boundary clean.

### Why no new image library

IIIF Image API's `{region}/{size}/{rotation}/{quality}.{format}` parameter grid maps cleanly onto AA's existing variant pipeline + ICC preservation + EXIF-orientation handling (per 1.18.A-2). Generating a 256x256 sRGB JPEG tile of region `0,0,1000,1000` of source asset X is the same work AA already does to generate a `preview.jpg` variant — just with operator-specified parameters instead of operator-config-specified ones. **The tile cache becomes the cache for these on-demand variants** so tiles served twice are served from the cache.

## Decision

AA implements IIIF as a parallel HTTP surface (`/iiif/2/...` for legacy Image API 2.0 redirect; `/iiif/3/...` for Image API 3.0 + Presentation API 3.0) backed by the existing variant pipeline.

### Image API (1.54.A shipped)

- `GET /iiif/3/{asset_id}/info.json` — image-info manifest per the spec
- `GET /iiif/3/{asset_id}/{region}/{size}/{rotation}/{quality}.{format}` — tile / region request
- Level 0 minimum (predefined sizes / fixed quality); Level 1 supported where the underlying variant pipeline natively handles it (region selection, size scaling, rotation modulo 90, format negotiation across JPEG/PNG/WebP)
- ICC profile preserved per 1.18.A-2 chunk-copy
- EXIF orientation applied via 1.18.A-2 orientation engine (source bytes pristine)

### Presentation API (1.54.B planned)

- `GET /iiif/3/collection/{collection_id}/manifest.json` — exposes a Collection per spec listing manifests for each member asset
- `GET /iiif/3/asset/{asset_id}/manifest.json` — single asset as a manifest with one canvas
- Multi-page assets (PDF, multi-asset posts) → canvases in sequence; Range structures for chapters / sections
- `navPlace` for geo-tagged assets (uses GPS extracted by 1.18.A-2)
- `metadata` block populated from AA's custom-field values per the extraction config

### Capability gate

`iiif.read` capability — seeded for the **anonymous** role so public assets / collections are visible to unauthenticated IIIF clients. Visibility composes with existing per-asset / per-collection visibility: a manifest request for a restricted asset returns 404 (not 403) to avoid leaking existence. Embargoed assets surface only their title in the manifest until embargo lifts.

### Caching

Two-tier cache per ADR-0013 caching pattern:

1. **`iiif.ManifestCache`** — collection / asset manifest JSON keyed by `(entity_id, version)` where version is the entity's `updated_at` (from ADR 0052 optimistic-concurrency). Invalidated on entity write OR collection-membership change. Heavy hit-path; LRU + Postgres LISTEN/NOTIFY broadcast for federation-ready invalidation.
2. **`iiif.TileCache`** — generated tile bytes keyed by `(content_hash, iiif_param_hash)`. LRU with operator-configurable size cap (`system_config.iiif.tile_cache_max_mb`, default 1024). Content-addressable forever — a tile of an asset's bytes never goes stale until the asset's bytes change.

No persisted tile derivatives on disk — tiles generated on-demand only; the cache handles the hot path. This avoids "regenerate all IIIF derivatives" maintenance burden that variant pipelines historically carry.

### Federation

Federated collections expose IIIF manifests that include canvases from federated assets. The canvas `id` field is the remote actor URI (per ADR 0043); IIIF clients (Mirador, Universal Viewer) fetch the remote canvas directly via that URI. AA does not proxy. **Cross-instance IIIF interop comes free** from the existing federation walled-garden surface.

### Standards version commitment

AA commits to Image API 3.0 and Presentation API 3.0 as the primary surface. Image API 2.0 endpoints are provided as redirects for compatibility with older Mirador deployments. **No support for Image API 1.x.**

### What this is NOT

- **Not a museum-management system.** IIIF gives institutional interop; collections / accession / provenance live elsewhere (Phase 1.51 physical archive mode).
- **Not a Mirador clone.** AA's viewer is its own surface (per `feedback_viewer_is_the_app`); IIIF gives external Mirador / Universal Viewer / OpenSeadragon access without us building them.
- **Not authenticated by AA's session cookie.** IIIF clients are typically anonymous or use bearer tokens per the IIIF Auth API; AA respects this and does not require its own session shape for IIIF requests.
- **Not a write surface.** IIIF is read-only in v1. Annotation write-back (IIIF Annotation Model 2.0) is a future consideration if operator demand emerges.

## Consequences

**Positive:**

- GLAM operators install AA and immediately have a Mirador / Universal Viewer / OpenSeadragon-compatible catalogue with zero custom-build effort.
- Cross-instance IIIF works out of the box — a federated AA peer's collection appears as a single Mirador manifest spanning instances.
- The existing variant pipeline absorbs all IIIF Image API workloads with no new image library or external dep.
- Tile cache is content-addressable forever; cache hit-rate approaches 100% on stable corpora.

**Negative:**

- New endpoint surface to maintain alongside the REST + ArchivePub surfaces.
- IIIF spec versions evolve; future major versions may require migration work. Mitigated by spec stability (3.0 is the long-term target; 4.0 is not on the horizon).
- Anonymous-by-default IIIF access requires careful visibility-gating tests (see acceptance criteria in 1.54.A PR).

## Alternatives considered

- **Skip IIIF; provide a custom AA-shaped public-catalogue API.** Rejected — institutional users need IIIF specifically (Mirador / Universal Viewer / scholarly tooling depend on it); a custom API solves nothing.
- **IIIF Image API only; skip Presentation API.** Rejected — Presentation API is what enables the Mirador interop story (collection manifests, multi-canvas viewing, ranges). Image API alone is a tile server without context.
- **Pre-generate all IIIF tile derivatives on upload.** Rejected — explodes disk usage; content-hash-keyed cache is sufficient and self-purges.

## Reference

- IIIF Image API 3.0 specification: https://iiif.io/api/image/3.0/
- IIIF Presentation API 3.0 specification: https://iiif.io/api/presentation/3.0/
- Mirador viewer: https://projectmirador.org/
- Universal Viewer: https://universalviewer.io/
- ADR 0034 — Capability add-ons (IIIF Auth API integration may ship as an add-on if operator demand emerges)
- ADR 0043 — Federation walled-garden protocol (cross-instance canvas URIs)
- ADR 0052 — Optimistic-concurrency edit-safety (`updated_at` drives manifest cache versioning)
- Phase 1.18.A-2 — Upload-side image metadata extraction (ICC + orientation surface IIIF consumes)
- RS-gap audit 2026-06-22 — original IIIF strategic-differentiator finding
