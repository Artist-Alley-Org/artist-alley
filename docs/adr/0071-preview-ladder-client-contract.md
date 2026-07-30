---
id: "0071"
title: The preview ladder is a published contract, not a shared constant
status: accepted
date: 2026-07-27
area: storage
phases: []
supersedes: []
related:
  - "0008"
  - "0011"
  - "0064"
tags:
  - storage
  - previews
  - api
  - frontend
excerpt: >-
  The set of raster variants an install generates is operator-configured,
  so neither the server nor the client may hardcode rung keys. The server
  publishes the configured ladder and a per-asset `ladder_available`
  flag; the client asks rather than assumes.
---

# 0071 — The preview ladder is a published contract, not a shared constant

## Context

The preview pipeline renders a **ladder** of raster variants per asset. The default is
`col` (320² cover), `preview` (1024 contain), `screen` (1920 contain), `hires` (4096
contain) — but the ladder is **operator-configurable** via sysconfig, and
`DefaultPreviewConfig()` is a default, not a contract.

For most of the project's life the client did not know this. Cards hardcoded
`/variants/col` as their only image URL, because `col` was the one rung they could assume
existed. That assumption cost real features:

- Responsive `srcset` was **deliberately disabled** in `PostCard` — the code and its
  `sizes` machinery were left in place with a comment saying they awaited a signal that
  did not yet exist. Every card served a 320px square regardless of viewport (#502).
- Widescreen art displayed as a square centre-crop, visibly disagreeing with its own
  hover-scrub animation, which used true aspect (#589).
- A proposal existed to add a *landscape-specific rung* to fix this — solving a
  knowledge problem with more storage.

The naive fix — publish the four default keys as a shared constant — reproduces the bug
one layer out. An install that drops `hires` to save storage, or adds a rung, would have
a client confidently requesting URLs that 404.

## Decision

**The ladder is published data, and neither side hardcodes it.**

1. **The server states what exists per asset.** `ladder_available` is true iff *every
   configured* rung is stored for that asset AND the caller passes the content plane
   (ADR 0064 — a restricted asset reports `false`, never 403, so the flag cannot become
   an oracle). It is computed against `sysconfig`'s configured variants threaded into the
   query as a parameter — never a literal list. One exported SQL fragment serves all call
   sites so the guard cannot be dropped in one of them.

2. **The server states what the ladder *is*.** `GET /previews` returns each rung's key,
   fit and `max_dim`. A flag saying "the whole ladder exists" is not actionable without
   knowing what the ladder contains: `srcset` needs the keys for URLs and `max_dim` for
   width descriptors.

3. **`GET /previews` is public-mode governed**, not unauthenticated. It is registered in
   `auth.PublicSurfaceRoutes`: anonymous on a public install, 401 on a private one.
   Deliberately **not** excused the way `/appearance` is — fonts render the login card, so
   an install that refused them could not draw its own sign-in page, whereas nothing
   before sign-in needs image rungs.

4. **The client caches the ladder and degrades to `col`.** A 401, an offline failure or a
   malformed response all mean the same thing: no ladder, use `col` only. The failure
   direction costs a feature, never a 404.

5. **`fit: cover` rungs are excluded from `srcset`.** `col` is a square crop; offering it
   as a width candidate for a contain-mode slot would letterbox or distort. The grid's
   contact-sheet mode still uses `col` directly and deliberately.

6. **The ladder's SOURCE shape is recorded per asset, in `asset_field_value`** under the
   existing `pixel_width` / `pixel_height` definitions, `set_by = 'computed'` (#757).

   The client can size a tile before any byte arrives only if the server states the shape.
   Points 1–5 tell it *which rungs exist and what they are*; none of them says *what shape
   the picture is*, and a layout that has to wait for `naturalWidth` reflows every tile on
   load. Masonry rendered a wall of identical squares for three merged PRs because that
   fact had a reader, an API field, a bucketer and a CSS rule — and no writer.

   **The quantity is the shape of the image the contain rungs are built from**, not "the
   source file's pixels". Half a catalogue has no source pixels: a 3D model, a font, an
   audio file and a plain-text document have none, yet each produces exactly one image on
   its way through the pipeline — a turntable frame, a glyph specimen, a waveform, a
   rendered plate — and fans it across the ladder. That image is what a card renders, so
   its shape is what a tile reserves. A 2048×384 waveform is a 5.33:1 tile; nothing in the
   file it came from says so.

   It is **not** recorded on `storage_variants`. That table is keyed by object hash and
   describes stored rungs, and the ladder source is not one of them — `col` is 320² for
   everything, so a per-variant row would either need inventing for an object that does
   not exist or would answer with the crop's shape rather than the picture's.

## Consequences

- **Adding or removing a rung is an operator action with no code change.** Both sides
  discover the ladder at runtime.
- **A hardcoded rung key anywhere is a bug**, on either side of the wire. This is the
  rule most likely to be violated by a future change that "just needs the hires URL".
- Landscape tiles required no new variant and no backfill — #589 collapsed into
  "request a different rung", which is the clearest evidence the diagnosis was right.
- `ladder_available` and `preview_available` are **different questions** and both are
  needed: the latter means "a servable `col` exists" (render a thumbnail at all), the
  former means "the full ladder exists" (safe to build a `srcset`).
- The flag is nearly co-extensive with `preview_available` on a healthy install — 1004 of
  1007 assets on the reference dataset have both. That is expected and not a reason to
  alias them: the distinction is what lets the client stop *guessing*, and it degrades
  correctly for partially-rendered or failed assets.
- **A handler that never reaches the ladder never records the shape.** Eight handlers
  short-circuit a non-forced re-queue before decoding anything (the same early exit that
  leaves a pre-#645 thumbhash unhealed), so the backfill for those is
  `aa rebuild-previews --force`. Raster and video are the exceptions — both reach the
  stamp on an ordinary re-queue, so they backfill without re-encoding a single rung.
- **The recorded pair is post-EXIF-rotation**, because the ladder source is. That is the
  shape a viewer sees and the shape the rungs were cut from; the EXIF extractor's own
  write is the on-disk pair, and where both have run the preview value is the better one.
- **Related trap, recorded in ADR 0008's amendment:** a derived variant under a stable
  content hash is exactly what preview regeneration rewrites, so ladder URLs addressed by
  *asset id* are not immutable. Cache validators must derive from the stored bytes.
