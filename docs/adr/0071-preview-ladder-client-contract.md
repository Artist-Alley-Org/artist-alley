# 0071 — The preview ladder is a published contract, not a shared constant

- **Status:** Accepted
- **Date:** 2026-07-27
- **Supersedes:** nothing
- **Related:** [0008](0008-storage-architecture.md) (content-addressed storage, amended 2026-07-27), [0011](0011-asset-entity.md) (asset entity, amended 2026-07-27), [0064](0064-visibility-two-planes.md) (row plane vs content plane)
- **Implemented by:** #610 (`ladder_available`), #613 (`GET /previews` gating), #636 (client consumption), #626 (`has_image` removal)

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
- **Related trap, recorded in ADR 0008's amendment:** a derived variant under a stable
  content hash is exactly what preview regeneration rewrites, so ladder URLs addressed by
  *asset id* are not immutable. Cache validators must derive from the stored bytes.
