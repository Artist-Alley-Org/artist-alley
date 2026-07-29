---
id: "0028"
title: PBR 3D viewer polish — IBL controls, material inspector
status: accepted
date: 2026-05-30
area: ux
phases: 
  - "1.18.B-10"
  - "1.18.B-16"
  - "1.18.B-8"
  - "1.36"
supersedes: []
related: 
  - "0017"
  - "0078"
tags:
  - ux
  - ai
  - 3d
excerpt: >-
  Phase 1.18.B-10 shipped the native 3D viewer (glTF / GLB / OBJ / FBX / Marmoset .mview, camera presets, IBL lighting, turntable poster, wireframe / UV inspect). The internal Marmoset-viewer reference and internal studio review processes both want a deeper PBR inspection layer: re…
---
## Context

Phase 1.18.B-10 shipped the native 3D viewer (glTF / GLB / OBJ / FBX /
Marmoset .mview, camera presets, IBL lighting, turntable poster,
wireframe / UV inspect). The internal Marmoset-viewer reference and
internal studio review processes both want a deeper PBR inspection
layer: real-time material parameter tweaking, IBL environment
switching from a curated set, per-material visibility, and direct
comparison between two materials on the same mesh.

This is the polish pass that makes the viewer a real tool, not a
preview.

## Decision

Add Phase 1.36 — PBR 3D viewer polish — extending the existing
`ModelView.svelte` host body with an inspector panel.

### Inspector panel

A right-side panel in the asset viewer's review mode:

- **Materials list.** Every material in the loaded model is listed
  with its texture maps. Click a material to focus it; per-material
  visibility toggle (hide / solo).
- **Per-material parameters.** Editable PBR params — base color,
  metallic, roughness, normal strength, emissive — with the change
  applied live to the WebGL scene. Resetting reverts the local
  override; the source asset is never written.
- **Texture map preview.** Each map (base color, normal, ORM /
  roughness, metallic, ao, emissive, opacity) has a thumbnail click-
  expand that opens the texture inspector (Phase 1.18.B-16) on that
  map specifically.
- **IBL environment selector.** Curated set of 8 HDR environments
  (studio / outdoor noon / outdoor sunset / overcast / indoor warm
  / indoor cool / dark room / pure white) plus a "use a project HDR"
  picker. Bound to the existing HDR / EXR preview pipeline.
- **Material comparison.** Pick two materials in the list, A / B
  wipe (reusing the 1.18.B-8 A/B compare primitives) over the same
  mesh.
- **Wireframe / UV / normal overlay.** Already shipped in 1.18.B-10;
  the inspector adds per-material wireframe (rather than whole-mesh).

### Output

The inspector's overrides can be exported as a `material_review.json`
file attached to the post — a structured comment that says "the
roughness on this material should be 0.4, not 0.7" without the
reviewer needing to write prose.

### Per-tier behaviour

PBR inspector is available to all tiers (same-feature, scale-only
differentiation per ADR 0017). The license tier caps the count of
loaded materials inspected in parallel via the tangled-derivation
model.

## Consequences

**Positive**

- The 3D viewer crosses from "preview" to "review tool" — actual
  art-direction work happens in-product.
- The override-as-JSON output is structured feedback that survives
  the conversation; engineers ingesting reviews don't have to
  parse comment prose.

**Negative**

- WebGL parameter binding to PBR params is non-trivial; uses three.js
  `MeshStandardMaterial` properties + a small reactive store.
- IBL environments need to be packaged with the binary (~30 MB
  HDRs) or fetched on first use. Mitigation: fetch-on-use with
  cache (saves binary size).

## Alternatives considered

- **Just embed the Marmoset Toolbag viewer for everything.** Marmoset
  is closed-source and would re-introduce a closed-source dependency
  we've otherwise avoided (ADR 0017). The native viewer covers most
  use cases; .mview shipping is already there for the cases where
  Marmoset is what the artist uses.
- **No live material editing; review-only.** Loses the highest-
  value flow.

## Reference

- Phase 1.36 in [`docs/roadmap.md`](../roadmap.md).
- 3D viewer (Phase 1.18.B-10, shipped).
- Texture inspector (Phase 1.18.B-16).
- A/B comparison (Phase 1.18.B-8).
