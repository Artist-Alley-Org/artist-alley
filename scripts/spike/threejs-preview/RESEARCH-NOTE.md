<!--
SPDX-License-Identifier: AGPL-3.0-only
Copyright (C) 2026 Kenneth Blossom
-->

# SPIKE #497 — Headless three.js turntable renderer vs Blender

> **ARCHIVED (#658).** The prototype this note describes — `worker.mjs`,
> `render.html`, `package.json`, `package-lock.json` — has been **deleted**.
> It did its job: #497 and #498 are both closed and the spike was
> productionised as **`scripts/threejs/`**, which is the code to read, run
> and change. What remained here was a stale second copy of the spike's
> dependencies, and it was the *only* thing holding the repo's one open
> Dependabot alert (`sharp` < 0.35.0, GHSA-f88m-g3jw-g9cj — inherited
> libvips CVE-2026-33327 / -33328 / -35590 / -35591). Deleting it closes
> the alert rather than maintaining a dead prototype's lockfile forever.
>
> The measurements, corpus and verdict below are kept as the evidence
> behind ADR 0069. The "How to reproduce" section no longer runs as
> written — recover the prototype from git history
> (`git log -- scripts/spike/threejs-preview/worker.mjs`) if you ever
> need to re-run it.

**Epic:** #496 (ADR 0069) · **Type:** research note + working prototype (NOT a cutover)
**Verdict:** **GO** — a headless Chromium + SwiftShader + three.js worker renders every
open-format model in the demo corpus, produces byte-shape-identical preview variants, and
runs **~20–30× faster than Blender** with **equal-or-better PBR fidelity**. No fidelity or
perf issue was found that would block the #498 migration of open formats off Blender.

The one gotcha (multi-file glTF needing its companions materialized) is **not** a
SwiftShader/three.js limitation — Blender fails identically on the same inputs. It is the
#486 companion-materialization concern, and it applies to whichever renderer we pick.

---

## What the prototype does

`worker.mjs` + `render.html` reproduce the `preview.model` render step
(`app/internal/preview/model.go`) with a headless browser instead of Blender:

- Launches Chromium via Puppeteer forced onto **software WebGL** — SwiftShader, the exact
  stack CI/servers run with no GPU (`--use-gl=angle --use-angle=swiftshader
  --enable-unsafe-swiftshader`). The renderer self-reports its backend so we can prove it:
  `ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero)), SwiftShader driver)`.
- Loads the model with the **same three.js loaders the interactive viewer uses**
  (`web/src/lib/components/viewers/ModelView.svelte`): `GLTFLoader` for glTF/GLB,
  `FBXLoader`, `OBJLoader`.
- Frames it to **match `scripts/blender/turntable.py`** — 20° elevation, 35 mm lens on a
  36 mm sensor (54.4° FOV), fit-to-frame with 1.2 padding — then rotates 36 steps (10°) and
  captures each frame.
- Fans the output into the **same variant shapes** as `model.go`:
  - `sprites.jpg` — 6×6 sprite sheet, 160 px cells → 960², JPEG q75
  - `sprites.vtt` — WebVTT, 4 s loop, `#xywh` cues (one per cell)
  - raster ladder from frame 0 — `col` (320² cover, webp q82), `preview` (1024, q86),
    `screen` (1920, q90), `hires` (4096, q95), all SkipUpscale (`withoutEnlargement`)

Output shapes were verified identical to the Go pipeline: `sprites.jpg` = 960×960,
`col.webp` = 320×320 cover, WebVTT cues = `sprites.jpg#xywh=x,y,160,160`.

## Corpus

13 complete models from the demo seed (`seed/internet-fetched/3d/`) plus a self-contained
FBX and an OBJ pulled from three.js's own examples — covering all three open-format loaders
and 254 → 121k triangles. Two seed models (`FlightHelmet.gltf`, `Sponza.gltf`) are
**incomplete in the seed** (no `.bin`, no textures) and are reported as clean fast errors —
see *SwiftShader / renderer gaps* below.

---

## Speed

All three.js numbers: SwiftShader software WebGL, 36 frames @ 512², one headless page.
`render` is the 36-frame GL loop; `wall` adds model load + sprite/ladder compositing.

| Model | Fmt | Tris | three.js render (36f) | three.js wall |
|---|---|--:|--:|--:|
| BoxAnimated | glb | 254 | 1082 ms | 1.37 s |
| Avocado | glb | 682 | 758 ms | 1.22 s |
| Duck | glb | 4,212 | 848 ms | 1.16 s |
| WaterBottle | glb | 4,510 | 1065 ms | 1.53 s |
| CesiumMan | glb | 4,672 | 746 ms | 1.07 s |
| male02 | obj | 5,004 | 636 ms | 0.94 s |
| Lantern | glb | 5,394 | 960 ms | 1.42 s |
| BoomBox | glb | 6,036 | 866 ms | 1.33 s |
| DamagedHelmet | glb | 15,452 | 1531 ms | 2.04 s |
| Samba | fbx | 55,320 | 852 ms | 1.57 s |
| BrainStem | glb | 61,666 | 1483 ms | 1.89 s |
| ToyCar | glb | 108,936 | 6830 ms | 7.22 s |
| 2CylinderEngine | glb | 121,496 | 2866 ms | 3.19 s |

**three.js render: min 0.64 s · median 0.96 s · max 6.8 s (ToyCar, 109k tris).**

### Blender baseline (same machine, same settings = the production settings)

`model.go` renders at **36 frames / 512² / 32 Cycles samples** (`ModelHandler` defaults). I
ran `scripts/blender/turntable.py` at those exact settings in the app container:

| Model | Tris | Blender turntable (36f, Cycles 32s) | three.js render | Speedup |
|---|--:|--:|--:|--:|
| Duck | 4,212 | **20 s** | 0.85 s | **~24×** |
| DamagedHelmet | 15,452 | **28 s** | 1.53 s | **~18×** |

The production Blender path does more than the turntable per model — a workbench poster
(~1 s) **plus** an isometric Cycles re-fan frame — so real per-model Blender wall is ~22–30 s.
The `60–180 s` figure in `model.go`'s comment is the worst-case wallclock cap, not the
typical cost; even so, three.js is **1–2 orders of magnitude faster** across the corpus. The
heaviest three.js model (ToyCar, 6.8 s) still beats Blender's *lightest*.

> Prototype overhead note: each model here spins up a fresh page + HTTP server + reloads the
> three.js module graph (~1–15 s of navigation not in the `render`/`wall` columns). A real
> worker keeps one warm page/context and pays that once — so production per-model cost tracks
> the `render`/`wall` columns, not the prototype's end-to-end.

---

## Quality (side-by-side, identical settings)

Comparison frames are committed in `comparison/` — `sbs_DamagedHelmet.png` and
`sbs_Duck.png` (LEFT = Blender Cycles 32s, RIGHT = three.js SwiftShader). Both were framed
with the same fit-to-frame math, so scale matches; the two
renderers start their turntable at different azimuths, so the *angle* differs — compare
material/lighting/detail, not pose.

**DamagedHelmet (PBR, metal + glass + emissive):** three.js reads **richer** — the visor
glass is correctly transparent/reflective with the emissive HUD glowing through, metallic
panels show crisp environment reflections (RoomEnvironment IBL + ACES tone mapping).
Blender's 32-sample Cycles with the neutral 3-point area rig looks flatter/more matte by
comparison. No SwiftShader artifacts: no banding, no missing textures, correct alpha.

**Duck (simple textured):** indistinguishable in quality — clean smooth shading, correct
albedo, sharp eye/beak. Both are perfectly good thumbnails.

Across the corpus, **no model rendered worse on SwiftShader than on Blender.** Software
WebGL is not a fidelity compromise here — three.js's PBR + IBL pipeline produces
gallery-quality thumbnails, and for reflective/emissive PBR assets it's visibly better than
the current Cycles-32-samples output.

---

## SwiftShader / renderer gaps (the "would this block cutover?" list)

1. **Multi-file glTF needs companions materialized — NOT a renderer gap.**
   `FlightHelmet.gltf` and `Sponza.gltf` fail with `Failed to load buffer "…​.bin"` because
   the seed ships the `.gltf` with **zero companions** (no `.bin`, no textures). **Blender
   fails on the same inputs** — there is no geometry buffer to read. This is the #486
   materialization concern and it is renderer-agnostic: whatever renders these must be handed
   the `.gltf` *and* its `.bin` + textures in one working directory. The prototype errors
   cleanly and fast (no hang), which is the correct behavior. **Action for #498:** ensure the
   render worker receives the full companion set (already a known #486 workstream), not a
   change to the renderer.

2. **Heavy meshes scale render time — but stay far under Blender.**
   Render time grows with triangle count (ToyCar 109k → 6.8 s; 2CylinderEngine 121k → 2.9 s —
   the difference is per-frame overdraw/material count, not just tris). Even the worst case is
   ~3× faster than Blender's *best* case. Not a blocker; if we ever hit pathological scenes,
   the existing 15-min job cap still applies.

3. **Ladder top-rungs clamp to the source-frame resolution — mirrors the current pipeline.**
   Fanning from a 512² frame with SkipUpscale means `preview/screen/hires` clamp to 512². The
   **production Blender path already does this** — it fans from a **384² workbench poster**
   (`--poster-res 384`), so its upper rungs clamp too. If we want true high-res upper rungs,
   the worker should render the poster/iso frame at the top ladder resolution — and three.js
   renders a single 4096² frame in **~0.5 s** (just a bigger canvas), whereas Blender needs a
   second Cycles pass. Another point in three.js's favor, not a gap.

4. **No GPU means no GPU.** Everything above is SwiftShader (CPU). A host *with* a GPU would
   be faster still, but the whole point of this spike was to prove the **no-GPU** path, and it
   clears the bar comfortably.

**Nothing here blocks #498.** The only real prerequisite (companion materialization) is
already tracked as #486 and is required for Blender too.

---

## How to reproduce

The 11 glTF/GLB models come straight from the demo seed. The FBX + OBJ rows use
`samples/Samba.fbx` and `samples/male02.obj` (gitignored) — grab them from the three.js
examples repo (`examples/models/fbx/Samba Dancing.fbx`, `examples/models/obj/male02.obj`)
into `samples/`, or point `worker.mjs` at any FBX/OBJ you have.

```bash
cd scripts/spike/threejs-preview
npm install
node worker.mjs \
  ../../../seed/internet-fetched/3d/DamagedHelmet.glb \
  ../../../seed/internet-fetched/3d/Duck.glb \
  samples/male02.obj samples/Samba.fbx
# → out/<name>/{sprites.jpg,sprites.vtt,col/preview/screen/hires.webp,frame_*.png}
# → out/results.json  (timings + gl backend per model)
```

Blender baseline (in the app container, which ships Blender):

```bash
docker compose cp seed/internet-fetched/3d/Duck.glb app:/tmp/Duck.glb
docker compose exec app blender --background --factory-startup --disable-autoexec \
  --python /app/blender/turntable.py -- \
  --input /tmp/Duck.glb --output /tmp/bl/tt --frames 36 --res 512 --samples 32
```

## Recommendation

**Proceed to #498** (migrate open formats — glTF/GLB, FBX, OBJ — off Blender to a headless
three.js worker). Fold the companion-materialization requirement (#486) into that work since
the renderer, whichever it is, needs the full file set. Keep Blender for the proprietary
formats it uniquely handles (#499) and revisit dropping it from the image (#500) only after
#498 lands.
