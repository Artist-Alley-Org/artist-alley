---
id: "0058"
title: Two-tier demo seed dataset (Layer A public / Layer B private)
status: accepted
date: 2026-07-12
area: ops
phases:
  - "1.22"
supersedes: []
related:
  - "0049"
  - "0034"
tags:
  - dataset
  - seed
  - dogfood
  - licensing
  - demo
  - screenshots
excerpt: >-
  The demo/seed dataset is split into two tiers. Layer A (site_a) is
  CC0/CC-BY/public-domain only — safe to redistribute publicly under an
  aggregate CC-BY-SA 4.0 license — and is the ONLY tier used for anything
  public: marketing screenshots, the hosted demo, the README, and public
  CI fixtures. Layer B (site_b) may carry IP-referenced or richer content
  and is private — dogfood + local testing only, never published.
---

## Context

Artist Alley needs a realistic, studio-shaped dataset to (a) dogfood
federation across two peers, (b) seed local dev + CI, (c) populate the
hosted demo once it exists, and (d) produce marketing + README
screenshots that show a *populated* DAM rather than an empty install.

The tension: the most compelling screenshots use rich, recognizable
media, but anything shown **publicly** (README, marketing site, hosted
demo, public CI artifacts) must be license-clean — we are publicly
displaying every asset in frame. Mixing "attractive but IP-encumbered"
content with "safe to publish" content in one dataset makes it
impossible to know which surfaces can go public.

## Decision

The seed dataset is **two tiers**, both curated under
`datasets/artist_alley/` (mounted on the self-hosted runner; a curated
subset is copied locally for screenshot work):

### Layer A — `site_a` (PUBLIC-SAFE)

- **"Artist Alley — Demo Studio Seed (Layer A)"** — composed
  **entirely** of open-source, public-domain, and Creative Commons
  content. No IP-referenced or personal content.
- **Aggregate license: CC-BY-SA 4.0** (forced by the most restrictive
  included source — the Wikimedia gameplay videos are CC-BY-SA; combined
  with CC0/CC-BY/PD content the aggregate is CC-BY-SA).
- Ships `MANIFEST.json`, `dataset-metadata.json`, and a canonical
  `ATTRIBUTIONS.md` listing every source + license. Sources include
  Kenney.nl (CC0 asset packs), Khronos glTF Sample Models (CC-BY/CC0),
  Wikimedia gameplay videos (CC-BY-SA), and other CC/PD material.
- Format coverage spans every asset type the app supports — images, 3D
  (GLB/OBJ/FBX), audio (OGG/WAV), video (WEBM), documents (EPUB), fonts
  (TTF), vectors (SVG), HDR — so a seeded instance exercises the full
  viewer range.
- **This is the ONLY tier used for anything public**: marketing-site
  screenshots, the OSS README screenshots, the hosted demo, and any
  public CI fixture. It is federation peer "site A" in the dogfood loop.

### Layer B — `site_b` (PRIVATE)

- May carry IP-referenced, personal, or otherwise not-publicly-
  redistributable content that makes for richer *internal* testing.
- **Never published.** Dogfood peer "site B" + local testing only. No
  screenshots from Layer B reach the README, the marketing site, or the
  public demo.

### Attribution obligation

CC-BY-SA 4.0 requires attribution + share-alike. Whenever Layer A
content (or a screenshot containing it) is redistributed publicly, the
`ATTRIBUTIONS.md` credit list must be preserved / linked. Practically:
the marketing site + README screenshot sections carry a "Demo content:
CC-BY-SA — see attributions" credit line pointing at the dataset's
attribution list.

## Consequences

**Positive:**

- A single, unambiguous rule for "can this go public": **Layer A only.**
  No per-asset licensing audit at screenshot time.
- Screenshots + hosted demo look genuinely rich (full format coverage)
  while staying license-clean.
- The dogfood loop's two peers (site_a / site_b) map cleanly onto the
  public/private split.

**Negative:**

- Layer A excludes the most recognizable IP, so screenshots skew toward
  generic-but-clean studio content (Kenney kits, sample 3D models). That
  is the correct trade for publishability.
- The attribution credit line is a standing obligation on every public
  surface that shows Layer A media.

## Alternatives considered

- **Single mixed dataset with per-asset license flags.** Rejected — a
  per-asset audit at every publish point is error-prone; one leaked
  IP-encumbered thumbnail in a README is a real problem.
- **Fully synthetic / AI-generated demo content we own outright.**
  Considered as a future option; the curated CC/PD dataset ships now and
  carries real format diversity a generator would have to reproduce.

## Reference

- ADR 0049 — Encrypted federation + dogfood (the two-peer loop site_a /
  site_b feed)
- ADR 0034 — Capability add-ons (heavy media the dataset exercises)
- `datasets/artist_alley/site_a/{MANIFEST.json, dataset-metadata.json,
  ATTRIBUTIONS.md}` — the canonical dataset provenance
