---
id: "0069"
title: Preview rendering via headless three.js; Blender demoted to proprietary-format converter
status: accepted
date: 2026-07-21
area: architecture
phases:
  - "1.47"
supersedes: []
related:
  - "0008"
  - "0028"
  - "0034"
  - "0038"
  - "0039"
tags:
  - architecture
  - 3d
  - preview
  - viewer
  - blender
  - rendering
excerpt: >-
  3D preview generation stays fully server-side and async, but the
  renderer changes: headless three.js driven by Puppeteer renders the
  turntable and poster, reusing the same code as the interactive
  viewer so previews are WYSIWYG. Blender is demoted from "renders
  every 3D thumbnail" to a converter invoked only for proprietary
  formats three.js cannot parse, and moves to an optional worker image
  so the base image sheds the heavy dependency.
---
## Context

Two things about the current 3D preview path prompted this decision.

**First, the interactive viewer is already native three.js.**
`web/src/lib/components/viewers/ModelView.svelte` renders glTF/GLB,
FBX, and OBJ entirely client-side via three.js loaders (WebGL, on the
viewer's own GPU); the server only streams the original bytes
(`/api/v1/assets/{id}/file`). So the common worry — "a Blender
instance will lag when many people view 3D" — rests on a
misunderstanding: **no server-side render happens at view time.** A
thousand concurrent viewers are a thousand browsers each rendering
locally, which scales like serving static files.

**Second, Blender is only in the *preview-generation* path — and it
is there for every 3D upload.** The `preview.model` job
(`app/internal/preview/model.go`) shells out to headless Blender to
render an N-frame turntable plus poster, cached as the raster ladder
variants and served on browse-grid cards. ADR 0039 assumed this same
Blender worker renders thumbnails for *every* 3D format, open and
proprietary alike (0039 Context and Layer 2). That has two costs:

1. **The preview is a different renderer than the viewer.** Blender
   Cycles output does not match what the user sees when they open the
   asset in the three.js viewer. The thumbnail and the interactive
   view diverge — not WYSIWYG.
2. **A heavy binary on the common path.** Blender is a large
   (~300 MB) dependency in the image, carries GPL-extension management
   and real operational friction (the release-image smoke test that
   repeatedly broke on write-permission and pinning), and it runs on
   *every* 3D upload even though the overwhelming majority are open
   formats (glTF/FBX/OBJ/STL/PLY/DAE/USDZ) that three.js parses
   natively. Blender's genuinely-needed job is the small minority:
   proprietary scene formats three.js cannot read.

The operator was explicit on two constraints: preview generation must
stay **server-side and async** (not offloaded to the user's client),
and Blender should be needed **only** for the formats that actually
require it.

## Decision

**Split rendering from conversion. three.js renders; Blender
converts — and only for what three.js cannot parse.**

1. **Preview generation stays 100% server-side and async.** It remains
   a job on the existing queue (`preview.model` and its siblings). No
   client-side thumbnail generation — the client never does preview
   work; it only renders for interactive viewing.

2. **The preview renderer becomes headless three.js, driven by
   Puppeteer.** A preview worker runs headless Chromium (WebGL via
   SwiftShader) and loads the *same* three.js scene/loader code the
   interactive viewer uses, renders the N-frame turntable plus the
   poster frame, and writes them back as the raster-ladder variants —
   exactly where the Blender job wrote them today. Because it is the
   same renderer as the viewer, the thumbnail and the live view match
   (WYSIWYG).

3. **Blender is demoted to a format converter, invoked only for
   formats three.js cannot parse** — the proprietary DCC scenes
   (`.max`, `.mb`, `.ma`, `.blend`, `.c4d`, `.hip`) and full USD. It
   converts the source to glTF; the headless-three.js renderer then
   renders that glTF like any other asset. This is precisely ADR
   0039's Layer 2, now scoped to **conversion**, with rendering handed
   to three.js. Open formats never touch Blender.

4. **The Blender worker moves behind an optional profile / separate
   image.** Operators who never ingest proprietary DCC formats never
   ship Blender. The base app and preview-worker images shed the
   ~300 MB dependency and its smoke-test surface; the Blender
   converter is an opt-in worker for the studios that need it.

5. **Puppeteer over headless-gl.** A headless three.js render needs a
   WebGL context in the worker. Two options: Node plus `headless-gl`
   (a light native module, but native-GL-in-a-container is finicky),
   or Puppeteer plus headless Chromium with SwiftShader (a heavier
   image, but exact client-WebGL parity and robust headless
   operation). We choose **Puppeteer** — fidelity and reliability win
   over image size, and the preview worker is already a separable
   image where the Chromium weight is acceptable.

### Relationship to ADR 0039

0039 stands — its three-layer proprietary-DCC strategy (free metadata
reader, free Blender converter, premium clean-room interactive viewer)
is unchanged in intent. This ADR revises **one assumption inside it**:
Layer 2's Blender worker is a *converter to glTF*, not the renderer of
the resulting thumbnail. The render of every format — including the
glTF that Blender emits for a `.max` file — is the headless-three.js
path. 0039 is amended accordingly (its Consequences bullet asserting
Blender renders every 3D thumbnail is struck and pointed here).

## Consequences

### Positive

- **WYSIWYG previews.** The browse-grid thumbnail is produced by the
  same renderer as the interactive viewer, so they match. No more
  Cycles-vs-three.js divergence.
- **One rendering codebase.** The viewer's loaders and scene setup are
  reused headless; a format that renders in the viewer renders in the
  thumbnail by construction.
- **Slim base image.** Blender leaves the default image; the ~300 MB
  dependency and its GPL-extension/smoke-test burden become an opt-in
  worker only proprietary-format operators deploy.
- **Faster common path.** three.js rasterization is far cheaper than
  Blender Cycles path-tracing on CPU; the vast majority of 3D uploads
  (open formats) get quicker previews on lighter workers.
- **Blender runs only when it is genuinely required** — proprietary
  scene conversion — which is also exactly where 0039 already wanted
  it.

### Negative

- **A JS/Chromium runtime enters the preview worker.** The preview job
  currently shells out to a Blender binary from the Go core; it will
  instead drive Puppeteer/Chromium (a Node runtime, or a Go-invoked
  headless-Chromium subprocess). New moving part to build and operate.
- **SwiftShader is software WebGL.** Fidelity should match the client
  (same three.js), but software rendering performance and any
  GPU-vs-software rendering differences must be validated against a
  real corpus before the Blender render path is removed. A spike
  gates the migration.
- **Two render substrates during migration.** Until the three.js
  preview worker is proven across the format matrix, Blender rendering
  stays as the fallback; there is a transitional period with both.
- **Very heavy scenes** that a browser WebGL context handles poorly
  (enormous vertex counts, exotic materials) may render worse headless
  than in Blender. The proprietary-viewer moat (0039 Layer 3) and the
  Blender converter still exist for those; the poster can fall back.

## Alternatives considered

- **Keep Blender as the universal thumbnail renderer (status quo /
  ADR 0039 as written).** Rejected: previews do not match the viewer,
  and a 300 MB dependency sits on the common path for formats that
  never need it.
- **Client-side thumbnail generation** (capture a frame from the
  viewer's three.js render and upload it). Rejected explicitly by the
  operator — preview work must be server-side and async, not pushed to
  the user's machine, and it must exist before the asset is ever
  viewed.
- **headless-gl (Node native WebGL) instead of Puppeteer.** Lighter
  image, but native-GL-in-a-container is fragile and diverges from the
  browser's WebGL. Rejected for reliability and fidelity; revisit only
  if Chromium image weight becomes a real constraint.
- **A dedicated software 3D rasterizer (Go/Rust).** Re-implements
  rendering, diverges from the viewer, throws away the WYSIWYG win.
  Rejected.

## Implementation

Sequenced so the slim-image win and WYSIWYG previews land before the
proprietary-converter build:

1. **Spike — Puppeteer preview worker.** Prove headless three.js
   (Chromium/SwiftShader) renders the turntable + poster for the open
   formats already supported (glTF/GLB, FBX, OBJ) at acceptable
   quality and speed, writing the same variant ladder the Blender job
   writes. Gate on a real-corpus comparison against current output.
2. **Migrate open-format previews off Blender.** Route open 3D formats
   through the three.js preview worker; keep Blender rendering only as
   a transitional fallback until the corpus is clean.
3. **Build the ADR 0039 Layer-2 Blender *converter* worker**, behind
   an optional profile / separate image: `.max` + `.ma` via
   `nrgsille76/io_scene_max` (GPL, in-container, black-box — see 0039);
   `.mb` via the opt-in `mayapy` build; USD via Blender's USD import.
   Output glTF, then render via the three.js worker. This is the
   "build the Blender extension" work the operator asked for.
4. **Drop Blender from the base image.** Once open-format previews no
   longer use Blender and the converter is a separate worker, remove
   the Blender dependency (and its smoke test) from the default build;
   the converter smoke test moves to the optional worker image.

Target: **all four before v1.0.0.** Steps 1–2 (WYSIWYG + slim image)
are the priority; steps 3–4 land the proprietary coverage and finish
the dependency removal.

## References

- ADR 0008 — Storage architecture. Preview variants and converted
  glTF are storage-backed variants on the asset.
- ADR 0028 — PBR 3D viewer polish. The client three.js viewer whose
  code the headless renderer reuses.
- ADR 0034 — Capability add-ons. The optional Blender-converter worker
  and any premium viewer ship as separable capabilities.
- ADR 0038 — Premium add-on layer. The proprietary interactive viewer
  (0039 Layer 3) is unaffected.
- ADR 0039 — Native DCC format viewers. Amended by this ADR: its
  Layer 2 Blender worker is a converter, not the thumbnail renderer.
