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
  formats three.js cannot parse. Amended 2026-07-27 (#500): Blender is
  not packaged at all — it left the image entirely (3.64 GB → 1.82 GB)
  and returns as a plugin, so the three.js worker is the only renderer.
  Amended 2026-07-29 (#689): "reusing the same code as the viewer" was
  aspirational — the renderer carried its own loader and rendered every
  OBJ and every unlit glTF untextured. The load path is now one shared
  module both surfaces import.
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

   *Amended 2026-07-29 (#689).* As shipped in #498, this was not true:
   `scripts/threejs/render.html` **reimplemented** the viewer's loader
   rather than importing it, and the copy had no `MTLLoader` and no
   material-upgrade pass. Every OBJ in the catalogue therefore rendered
   as untextured white, and every `KHR_materials_unlit` glTF as a flat
   unlit silhouette, while the viewer showed both correctly — for two
   releases, behind comments in both files asserting the opposite. The
   load path (loader dispatch, OBJ `.mtl` resolution, material
   normalisation) and the default lighting constants now live in
   `web/src/lib/3d/modelLoader.js` and `defaultLighting.js`, which the
   viewer imports directly and which `worker.mjs` serves to `render.html`
   at `/shared/` (the Dockerfiles copy them next to the worker). The
   canonical home is under `web/` because the dev web container
   bind-mounts only `./web` — that constraint is why the code was
   duplicated in the first place, and serving the files from the worker
   is what dissolves it. The lesson: a comment claiming a structural
   guarantee is worse than no comment, because it stops anyone looking.

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

### Amendment 2026-07-27 (#500): Blender is not packaged at all

Step 4 below said Blender "moves behind an optional profile / separate
image" — still *packaged*, just not in the default image, and still the
Layer-2 converter that ships with the product. **That is no longer the
plan. Blender is not packaged by Artist Alley in any image. It becomes
a plugin.**

The measurements that forced the change, taken on the v0.6.0 image
before this landed:

- `/opt/blender` was **1.3 GB of a 3.64 GB image (~36%)**. Removing it
  and the GL/X libraries it dlopened took the compose runtime image to
  **1.82 GB — a 50% cut**, larger than Blender itself because the
  `libgl1 / libegl1 / libxrender1 / libxi6 / libxxf86vm1 / libxfixes3 /
  libxkbcommon0 / libxext6 / libsm6 / libice6` set and `xz-utils` went
  with it.
- The 3D catalogue is **`glb` (116), `obj` (105), `fbx` (105),
  `gltf` (2)** — every one of which has routed to the three.js worker
  since #498. A query for the Blender-only formats
  (`md2/md3/mdl/ms3d/mview`) across the whole catalogue returns
  **none**.

So the "transitional fallback" was not a fallback. It was 1.3 GB of
dead weight that no upload could reach, on the *common* path, for a
capability nobody was using.

What this changes concretely:

1. **There is exactly one renderer.** A format outside the three.js
   loader set gets no turntable — a logged, terminal-free skip, not a
   job failure. The upload is intact and served; only the generated
   thumbnail is absent, and marking an asset `failed` over a missing
   thumbnail would be a lie.
2. **A missing worker is a deployment fault, not a degraded mode.**
   With nothing to fall back to, `preview.model` returns a *retryable*
   error when the worker is absent rather than marking the asset ready
   with no preview. The `DisableThreeJS` escape hatch is **removed**:
   it existed to force the Blender path, and "disable the only
   renderer" is not an operation worth offering. Nothing ever set it —
   no env var, no `system_config` key, only a test.
3. **`stl`, `ply` and `dae` moved onto the worker.** Blender used to
   produce their thumbnails. This ADR already listed them among the
   formats "three.js parses natively", so they are picked up via the
   stock loaders rather than losing previews.
4. **`blend`, `x3d`, `wrl`, `usd*`, `abc` have no renderer** until the
   converter plugin ships (#499, re-scoped to plugin delivery and moved
   to v0.14.0). They are the formats the catalogue does not contain.
5. **The release-image smoke test survives, re-pointed.** Its purpose —
   proving the 3D chain works *in the built image* — outlives Blender,
   because what can now go missing (Chromium's dlopen deps, the
   `node_modules` copy, the importmap paths, a loader that moved in
   three's addons) still fails at **render** time, where a clean build
   proves nothing. `scripts/threejs/smoke.mjs` drives the real chain
   once per supported format and asserts the poster has non-transparent
   pixels, since an empty render is what a broken loader looks like on
   disk. The Blender pin-drift gate (#470) is likewise re-pointed at
   the renderer that replaced it rather than deleted.

   ⚠️ *Amended 2026-08-12 (#1049) — **this section names the smoke test
   as the enforcement mechanism but never says when it runs, and its
   coverage is narrower than a reader will assume.*** The test lives in
   `ci.yml`'s `docker-build` job, which carries
   `if: github.event_name != 'pull_request'` — so it runs on **no pull
   request at all**. That is deliberate (a full production image build
   per PR is expensive), and #751 recorded it, prescribing "verify
   locally with an image build" as the compensating control. What broke
   on 2026-08-12 is the other half: a PR merged by the Dependabot
   automerge workflow is merged with `GITHUB_TOKEN`, and GitHub does not
   trigger workflows on such pushes — so **no push run follows the merge
   either**. Three dependency PRs, one of them raising the worker's
   `three` by sixteen minor versions, reached `dev` with this gate having
   run **nowhere**. The renderer was verified by hand (smoke 10/10 on a
   freshly built image at `217766f2`) and is fine, but the guarantee this
   section promises was unenforced for that window. **Do not read "the
   smoke test survives" as "every change to the render chain is gated."**
   #1049 tracks closing it.

   ✅ *Closed 2026-08-12 by #1049 (PR #1051), same day.* The gate is now
   reachable from both directions. `scripts/ci/render-chain-paths.txt` is
   the single source of truth for "does this change put the render chain
   at risk?"; `ci.yml`'s `render-chain` detector job gates `docker-build`
   on it for pull requests, and `dependabot-automerge.yml` refuses to
   auto-merge a matching PR unless `Verify production image build`
   concluded `success`. Both readers fail **closed**. The gate was proven
   in all three states before merging — red on a real PR with a broken
   `/shared/` import (the failing step was `Assert 3D preview smoke`,
   every other job green), skipped on a non-matching change, and green on
   its own PR, where `Verify production image build` passed on a pull
   request for the first time.

   **The correction that matters for anyone reading this section later:**
   the release image `docker-build` builds is the **root `Dockerfile`**
   (`file: Dockerfile`), *not* `infra/docker/app/Dockerfile`. Those are
   two files whose runtime stages diverging **is** the #470 bug, and the
   distinction is easy to miss — a hand-verification during this arc built
   the wrong one. The path list carries both, and says which is which.
   It also carries `web/src/lib/3d/*`, because `render.html` does not own
   its loader: it imports `modelLoader.js` and `defaultLighting.js`, which
   is the seam this ADR's #689 amendment is about, and a gate blind to
   that directory would miss the exact regression it exists to catch.

`scripts/blender/turntable.py` and `ab_engine_test.py` are deleted with
this change. They are recoverable from git history, but the plugin
(#499) should be written against the Blender and worker contracts
current at that time, not resurrected from a 4.2-era script wired to a
ModelHandler that no longer exists.

## Consequences

### Positive

- **WYSIWYG previews.** The browse-grid thumbnail is produced by the
  same renderer as the interactive viewer, so they match. No more
  Cycles-vs-three.js divergence.
- **One rendering codebase.** The viewer's loaders and scene setup are
  reused headless; a format that renders in the viewer renders in the
  thumbnail by construction. *(Amended 2026-07-29, #689: true only
  since the shared `modelLoader.js` landed. The smoke test now asserts
  it — every material must arrive as `MeshStandardMaterial` and the
  textured fixtures must render textured, because "the poster has
  non-transparent pixels" is a check flat white geometry passes.)*
- **Slim base image.** Blender leaves the default image. *(Amended
  2026-07-27, #500: it leaves the product entirely and becomes a
  plugin, and the dependency measured 1.3 GB rather than the ~300 MB
  estimated here — 3.64 GB → 1.82 GB once its GL/X libraries went too.
  The GPL-extension burden leaves with it.)*
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
  gates the migration. *(Done — #497/#498.)*
- **Two render substrates during migration.** Until the three.js
  preview worker is proven across the format matrix, Blender rendering
  stays as the fallback; there is a transitional period with both.
  *(Closed 2026-07-27, #500. The transitional period is over: there is
  one substrate. See the amendment above — the fallback turned out to
  be unreachable, because every format in the catalogue routes to the
  worker.)*
- **Very heavy scenes** that a browser WebGL context handles poorly
  (enormous vertex counts, exotic materials) may render worse headless
  than in Blender. The proprietary-viewer moat (0039 Layer 3) still
  covers those. **There is no longer a renderer to fall back to** —
  amended 2026-07-27 (#500): a scene the worker cannot render produces
  no turntable, and the asset is still served and viewable. Do not read
  the sentence this replaced ("the poster can fall back") as a live
  capability; it never shipped as one.

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
3. **Build the ADR 0039 Layer-2 Blender *converter*** — `.max` + `.ma`
   via `nrgsille76/io_scene_max` (GPL, in-container, black-box — see
   0039); `.mb` via the opt-in `mayapy` build; USD via Blender's USD
   import. Output glTF, then render via the three.js worker. This is
   the "build the Blender extension" work the operator asked for.
   **Amended 2026-07-27 (#500): it ships as a plugin, not as a worker
   image we package.** #499 is re-scoped to plugin delivery and moved
   to v0.14.0.
4. **Drop Blender from the base image.** ~~Once open-format previews no
   longer use Blender and the converter is a separate worker~~ —
   **done 2026-07-27 (#500), and ahead of step 3 rather than after
   it.** Waiting for the converter would have meant carrying 1.3 GB of
   unreachable dependency through however many releases step 3 takes;
   the catalogue showed nothing was using it. Removed: the Blender
   download/verify/extract from both Dockerfiles, the `/app/blender/`
   layout and `turntable.py`, the GL/X libs it dlopened, the Blender
   render/poster/isometric paths in `ModelHandler`, and the
   `DisableThreeJS` escape hatch. The release-image smoke test was
   **re-pointed, not deleted** — see the amendment above.

Target: **all four before v1.0.0.** Steps 1, 2 and 4 have landed
(#497/#498/#500); step 3 lands the proprietary coverage as a plugin.

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
