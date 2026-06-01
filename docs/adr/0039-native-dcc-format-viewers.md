---
id: "0039"
title: Native viewers for proprietary DCC formats — clean-room, Blender-augmented, premium
status: accepted
date: 2026-05-31
area: architecture
phases:
  - "1.47"
supersedes: []
related:
  - "0008"
  - "0011"
  - "0028"
  - "0034"
  - "0036"
  - "0038"
  - "0040"
tags:
  - architecture
  - 3d
  - dcc
  - viewer
  - reverse-engineering
  - premium
excerpt: >-
  Inventory, thumbnail, and interactive viewing for proprietary DCC
  scene files (`.max`, `.mb`, `.ma`, etc.) ship across three
  deliberate layers — a free Go metadata reader, a free Blender
  worker for thumbnails + conversion, and a premium `aa-dcc-viewer`
  add-on holding the proprietary routing algorithm + native parser
  + material translation. Clean-room methodology is load-bearing.
---
## Context

The Phase 1.18 "Universal asset viewer" arc has native in-browser
viewers for every open 3D format we care about — glTF, OBJ, FBX,
USD, Marmoset `.mview`, the legacy game formats — and Blender-
rendered turntable thumbnails for everything else. The gap is the
**proprietary scene formats**:

- `.max` — Autodesk 3ds Max scene. OLE2 / CFBF container; per-class
  TLV chunk tree; chunk IDs resolved through a `DllDirectory`
  manifest, meaning payload semantics are **plugin-DLL-dispatched**
  and effectively opaque without the right DLL. Textures are never
  embedded, only path-referenced.
- `.mb` — Autodesk Maya binary scene. EA Interchange File Format
  (IFF) — documented chunked structure, no DLL-dispatch mess.
- `.ma` — Autodesk Maya ASCII scene. Parseable MEL script.
- Follow-on (out of v1 scope): `.hip` (Houdini), `.spp`
  (Substance Painter), `.zpr` (ZBrush).

None of these can be rendered or even reliably introspected by
any code that lives outside the originating DCC today. ResourceSpace
treats them as opaque blobs — operators upload them, RS stores them,
no thumbnail, no metadata, no preview. The viewer just shows a generic
"3ds Max file" icon. For a studio whose canonical assets are `.max`
scenes, that is a wholesale failure of the asset-management value
proposition.

### What exists in the wild

A research pass through prior art turned up:

- **`nrgsille76/io_scene_max`** — Blender 4.2+ extension, Python,
  **GPL-2.0+**, ~2,200 LOC, actively maintained (last push 2026-05-22).
  Covers Editable Mesh / Poly, Standard / Physical / V-Ray / Corona /
  Arch & Design materials, bitmap refs, bones, cameras, lights.
- **Kaetemi blog series (parts 1–5)** — the only written spec for
  `.max`'s internal structure. Reverse-engineering notes, publicly
  available, narrative not code.
- **`jmplonka/Importer3D`** — FreeCAD predecessor, narrower, useful
  as a learning artifact.
- **Neil's blog + David Duber's repathing post** — scope is
  `FileAssetMetaData2` only (asset-path enumeration).
- **`mayabin2ascii` + `maya-binary-parser`** — `.mb` parsing prior
  art. Autodesk publishes a partial `.mb` spec.

No Go, Rust, or C++ `.max` or `.mb` parser exists today. Anything
written in our stack is greenfield.

### What we need

Three distinct capabilities, in roughly this priority:

1. **Metadata at upload time.** Inventory: textures referenced,
   XRefs, plugin manifest, scene units, modified-by-user, last-saved
   version. Answers "what's in this file?" without paying a Blender
   cold-boot cost. Useful even on installs that never view the file.
2. **Thumbnail at upload time.** A turntable preview of the scene
   for the browse-grid card. Acceptable to be a few seconds of
   worker time; not acceptable to be missing.
3. **Interactive in-browser viewer.** Scrub through the scene at
   review time — orbit, pan, zoom, hide-show layers, inspect
   materials. The actual differentiator: nobody else does this for
   `.max` outside Max itself.

Capability 1 has near-zero legal risk and ships in a weekend.
Capability 2 is months of risk-free engineering against an existing
GPL Blender extension. Capability 3 is **the moat** — the place
where proprietary work pays for itself and where reverse-engineering
discipline matters.

### The licensing trap

The lowest-effort path to capability 3 is "port `io_scene_max` to
Go." Three problems:

1. **GPL contamination.** `io_scene_max` is GPL-2.0+. A Go port is
   a derivative work. Premium add-ons under ADR 0038 ship as
   closed-source under EULA — a GPL-derivative add-on is a license
   violation, ours not Autodesk's.
2. **Multi-month sink with silent-corruption failure modes.** Chunk-
   ID collisions across plugin contexts, compressed-stream handling,
   unit scale, Z-up vs Y-up, missing-plugin payload skips. The
   `.max` format is genuinely hard.
3. **Format drift exposure.** Every new 3ds Max release ships plugin
   updates that shift chunk meanings. A native parser is on the
   format-drift treadmill forever; a Blender worker rides Blender's
   community-maintained extension instead.

The right answer is **don't port; reuse where the license lets us;
build proprietary only where we earn value**.

### The "patching" framing

A concern surfaced in the strategy thread is that Autodesk could
patch the `.max` format to break third-party viewers if our work
becomes visible. This is largely a phantom risk:

- Format changes break Autodesk's own customer base; they don't
  ship them gratuitously.
- The natural moat is the plugin-DLL dispatch — opaque payloads
  without the DLL. That moat exists whether or not our code is
  secret.
- The actual risk vector is **GPL contamination + reverse-engineering
  provenance**, not Autodesk's roadmap.

Secrecy serves three real purposes:
1. **Revenue moat** — no OSS fork competes on our code.
2. **Reduced legal attack surface** — proprietary binary + EULA +
   `.lic` gate means a challenger has to extract IP to dispute it.
3. **Clean-room provenance hygiene** — we can demonstrate the
   methodology privately if challenged.

ADR 0038's premium-add-on infrastructure already gives us all three
without any new mechanism.

## Decision

Ship native DCC scene support across **three deliberate layers**.
Two are free + in-tree under the AGPL core. One is a premium add-on
under ADR 0038 holding the proprietary work.

### Layer 1 (free, in-tree): `dcc-fileinfo` Go metadata reader

In-process Go package at `internal/dcc/fileinfo`. Parses the OLE2 /
CFBF container of `.max` files using a CFBF library (`mscfb` or
similar — the format is Microsoft Compound File Binary, public
spec). Enumerates the documented streams:

- `FileAssetMetaData2/3` — texture / XRef / asset references with
  path + flags. (Documented by Autodesk in KB articles; this is
  the public, supported access path.)
- `SummaryInformation` — author, modified-by, last-saved version.
- `DllDirectory` — plugin manifest (plugin-name + version pairs);
  surfaces the "we'd need plugin X to fully render this" answer
  without trying.
- Embedded thumbnail if present (older `.max` files carry a JPEG
  preview; newer files don't).

`.mb` parsing follows the documented EA IFF chunked-format spec —
we walk the chunk tree without inferring undocumented semantics.
`.ma` parsing is a MEL tokenizer.

**No chunk-semantic interpretation, no GPL exposure.** Every
information channel this layer exposes is one Autodesk has either
documented or that exists in an open container standard. Output is
JSON via a Go API and a CLI (`dcc-fileinfo path/to/scene.max | jq`).

This layer ships immediately and gives every install — Community
included — a real answer at upload time:

- File card shows author, last-saved-version, plugin requirements.
- Texture list is enumerable for orphan detection and reference
  resolution.
- Plugin-missing warnings ("This scene references V-Ray; install
  the V-Ray add-on for full preview fidelity") become possible.

### Layer 2 (free, in-tree): Blender worker extension for thumbnails + conversion

The existing thumbnail-worker pattern from Phase 1.18 (Blender
container behind `--profile workers`) extends to handle `.max` and
`.ma` by installing the open-source Blender extensions inside the
worker:

- `.max` — `nrgsille76/io_scene_max` (GPL-2.0+). Blender extensions
  install into the worker container at build time; the worker is
  invoked as a subprocess from the Go core, output is glTF or USD
  written back to the filestore.
- `.ma` — Blender's MEL-bridge import path.
- `.mb` — deferred to paid-customer demand. Blender does not import
  `.mb` directly today; `mayapy` would be required (free with a
  Maya install), which gates this on the operator having Maya
  licensed. Not blocking for v1.

**The GPL plugin stays inside the Blender container.** Our Go core
calls Blender via the existing subprocess pattern (this is the AGPL-
compatible call boundary already established for Blender turntables).
The output is glTF / USD — universal, fork-safe, ingestible by
*anything*. No GPL code is linked into our binary or our premium
add-ons.

Thumbnails generated this way are cached as preview variants on the
asset and served the same way every other 3D thumbnail is served.
The `dcc-fileinfo` metadata (Layer 1) is captured at the same time
in the worker pipeline and stored on the asset row.

This layer covers **thumbnail + offline conversion to glTF/USD** for
every `.max` and `.ma` an operator uploads. For the operator who
needs Maya in this loop, an opt-in build of the worker container
includes `mayapy`; that build ships under a separate Dockerfile and
is not distributed by us (the operator builds it with their Maya
license).

### Layer 3 (premium add-on `aa-dcc-viewer`): the interactive viewer + proprietary routing

The proprietary part. Premium add-on per ADR 0038. Closed source,
EULA-licensed, Ed25519 `.lic`-gated.

Three sub-pieces, all proprietary:

- **Native Go parser for the common-case scene shape.** Geometry,
  transforms, basic Standard / Physical materials, cameras, lights,
  layer hierarchy. Handles the 80% of chunks the Kaetemi spec
  documents structurally. Built clean-room from the public spec —
  never reading `io_scene_max` source.
- **The routing algorithm — the user's IP.** Per-chunk decision
  layer that picks between:
  - Native parsing (fast, interactive, in-browser).
  - The cached Blender-glTF artifact for that chunk subtree
    (slower upfront but full fidelity, GPL-derived but accessed as
    a glTF artifact not as code).
  - Skip-with-structural-hint (opaque blob from a plugin we don't
    recognise — preserve the structural placeholder for the viewer
    to render as "missing").
  The routing intelligence is a per-chunk-ID heuristic database
  built up over time from observed `.max` files. This is the actual
  moat: better-than-Blender interactive performance for the common
  case, Blender fidelity for the hard case, graceful degradation for
  the unknown case. **The heuristic database is the trade secret.**
- **Material translation layer.** V-Ray / Corona / Arch & Design /
  Standard / Physical → glTF PBR with documented approximation
  rules. Customers get a viewer that respects their materials
  rather than showing untextured gray meshes.

The add-on ships as a Docker container in the existing capability
add-on registry (Phase 1.42 / ADR 0034). It exposes a stable HTTP
interface to the AGPL core; the core has zero knowledge of the
proprietary algorithm. The viewer in the browser is the existing
Universal Asset Viewer (Phase 1.18.B-2) talking to whichever live-
viewer slot is bound — `aa-dcc-viewer` satisfies a `dcc-live-viewer`
capability slot.

### The B/C mix — what it actually means

Tier B in the strategy doc was "port to Go." Tier C was "headless
Blender worker." A naive B-or-C split throws away the strengths of
both:

- Pure C means every viewer interaction round-trips through Blender.
  Cold-boot cost, queue latency, fidelity-overkill for simple
  scenes. Wrong UX for review.
- Pure B means every chunk has to be reverse-engineered before any
  viewer works. Multi-month before the first asset opens.

The B/C mix is "**B for the cheap-and-common, C for the heavy-and-
weird, routed at chunk granularity by an algorithm we own**." The
viewer opens in milliseconds with a partial scene; the routing
algorithm streams in heavy chunks from cached Blender glTF as
needed; the user perceives a fast viewer that gracefully fills in.

The cached Blender glTF is **Layer 2 output**. Layer 3 consumes
those glTF artifacts as data, not as code — glTF is a documented
open format. Our consuming code (Layer 3) is fully proprietary and
GPL-free because the input it sees is data, not source.

### Clean-room methodology (load-bearing)

Every contributor to Layer 3 follows these rules. They are not
suggestions:

1. **Read the Kaetemi blog series + Autodesk KB articles freely.**
   These are spec documents — descriptions of structure, not
   implementations. Reading them does not contaminate.
2. **Do not read `io_scene_max`, `Importer3D`, `mayabin2ascii`, or
   `maya-binary-parser` source.** These are GPL or unclear-license
   implementations. Anyone who has read them in detail is excluded
   from contributing to Layer 3 for a documented quarantine period.
3. **Use the Blender worker as a black-box oracle.** When a chunk
   isn't structurally clear from the spec, feed the file to the
   Blender worker, observe the glTF output, work backward to the
   chunk's semantic role from observed effects. This is observation
   of behaviour, not code reading.
4. **Document the methodology per chunk-ID.** Each entry in the
   heuristic database links to the spec passage or the observation
   that justifies it. Provenance survives challenge.
5. **Test against the same `.max` files Blender does.** Validation
   is "did we produce the same node tree Blender did?" against a
   suite of test files. The test suite is the cross-check, not the
   source.

A future audit can demonstrate clean-room provenance from these
artifacts. The provenance is the legal defence; the secrecy is the
revenue defence; both compose.

### Free-vs-paid line (per ADR 0038)

Layer 1 (`dcc-fileinfo`) is free in core because: every operator
benefits, including those who never use the viewer; the work has no
proprietary value (it's just reading public formats); shipping it
free reduces the perception that paid add-ons hide value the
Community tier deserves.

Layer 2 (Blender worker) is free in core because: it's a Blender
subprocess pattern we already established for turntables; the work
is mechanical (install extensions, plumb the worker invocation);
the legal cleanliness comes from the GPL plugin running in its
own process. Free here also makes the Layer 3 upgrade story honest
— operators get *something* without paying; Layer 3 buys them the
*interactive* viewer.

Layer 3 (`aa-dcc-viewer`) is paid per ADR 0038 because: the routing
algorithm + heuristic database is operational burden we carry on
the operator's behalf (we track format drift, accumulate the
heuristic corpus, ship updates); customers who need it pay for it;
locally-equivalent free path exists (Layer 1 + Layer 2 thumbnail +
"Open in Max" for the rare interactive need) so the gate is honest.

### Sub-phase rollout (Phase 1.47)

- **1.47.A — `dcc-fileinfo` metadata reader** (free, in-tree).
  Go package + CLI. `.max` + `.mb` + `.ma`. Wired into the upload
  pipeline so every uploaded scene file gets its metadata captured
  at ingest. Ship in days.
- **1.47.B — Blender worker extension** (free, in-tree). Worker
  container build with `io_scene_max` installed; `.max` + `.ma`
  thumbnail + glTF conversion path. Plugin-missing graceful
  degradation surfaced through Layer 1's plugin manifest. Ship in
  weeks.
- **1.47.C — `aa-dcc-viewer` premium add-on** (paid, proprietary).
  Native Go parser for common-case chunks + routing algorithm +
  material translation + live-viewer slot binding. Clean-room
  build. Ship when the algorithm is robust enough to be honest
  about; not before.
- **1.47.D — Extended format coverage** (deferred, demand-driven).
  Same three-layer pattern for `.hip` (Houdini) / `.spp`
  (Substance Painter) / `.zpr` (ZBrush). Each format is its own
  sub-phase; each ships only when a paying customer asks.

## Consequences

### Positive

- Every operator — Community included — gets real answers about
  proprietary scene files at upload time. The "DCC scene as opaque
  blob" failure mode disappears at Layer 1.
- Thumbnails work for every `.max` and `.ma` without paid add-ons.
  Layer 2 ships with the AGPL core. The browse grid shows real
  previews of `.max` files; that alone is a wholesale value
  delivery against RS.
- The proprietary work sits in a single, well-bounded artifact
  (Layer 3) that is legally cleanly separable, commercially
  defensible, and operationally upgradeable without touching the
  core.
- The B/C-mix routing is a real engineering moat. Pure-Blender
  competitors have unacceptable interactive latency; pure-native
  competitors have unacceptable format coverage. The routing
  algorithm earns its keep on every viewer interaction.
- Clean-room methodology documented up front survives any future
  challenge. Provenance is the legal defence; the architecture
  makes the provenance demonstrable.
- ADR 0038's premium-add-on infrastructure does all the heavy
  lifting for distribution / licensing / billing. Layer 3 ships
  through machinery that already exists.

### Negative

- Three layers is more architecture than a one-shot "Blender worker
  for everything." The added complexity is real and has to be
  documented for contributors (clean-room rules; layer boundaries).
- Layer 3's value depends entirely on the routing algorithm being
  genuinely better than pure Blender. If we ship a viewer that
  feels like Blender-with-extra-steps, the premium pricing won't
  hold. Engineering risk is real.
- The heuristic database is the moat — and the moat erodes if it
  leaks. Internal handling discipline matters (no public dumps,
  per-customer accumulation discouraged, build artifacts
  obfuscated).
- Clean-room methodology slows development. Contributors can't
  shortcut "let me just look at how `io_scene_max` does this."
  The quarantine rule excludes anyone who has read the GPL
  implementations in detail.
- `.mb` is deferred at v1 because Blender doesn't import it
  directly. Maya-shop customers will ask for it; we have to be
  ready to say "deferred, demand-driven" without it becoming a
  perpetual blocker.
- Format drift is now our long-term operational obligation per
  paying customer. Every new 3ds Max release potentially shifts
  chunk meanings; we have to maintain the heuristic database
  against the drift. This is the burden we charge for; it has to
  be actually carried.

## Alternatives considered

- **Tier A only — metadata reader, no viewer.** Ship the cheap
  win, skip the viewer entirely. Rejected because the viewer is
  the differentiator; "metadata-only" is RS's current floor and
  losing on UX value.
- **Tier C only — Blender worker for everything, including the
  interactive viewer.** Cold-boot latency makes interactive review
  unusable. Acceptable for thumbnails (Layer 2); unacceptable for
  live viewer. Rejected.
- **Tier B only — port `io_scene_max` to Go, ship as proprietary
  add-on.** GPL contamination kills this; Go-port-of-GPL is GPL.
  Rejected on license grounds independent of effort.
- **Pure clean-room Tier B from spec, no Blender at all.** Months
  of work; silent-corruption failure modes; on the format-drift
  treadmill alone forever. Rejected as the v1 default; permitted
  as Layer 3's common-case path with Layer 2 as the heavy-case
  fallback.
- **Outsource to Autodesk Forge / Vault SDK.** Autodesk's hosted
  viewer service. Rejected: vendor lock-in, self-hosting
  commitment (ADR 0006), per-API-call costs that don't compose
  with our pricing model, and Autodesk could withdraw service at
  any time.
- **Wait until WASM plugins ship and let the community write a
  `.max` viewer plugin.** Defer Layer 3 to Phase 1.23 plugin
  ecosystem. Rejected because the WASM plugin layer is years out,
  and DCC scene support is one of the largest existing-customer-
  base value propositions. Premium add-on now; community plugins
  later if the demand outlasts our add-on.

## Implementation

Sub-phases land sequentially under Phase 1.47.

- **1.47.A — `dcc-fileinfo` metadata reader.** Go package
  `internal/dcc/fileinfo` with subpackages per format. CFBF
  parsing via a vetted library. `FileAssetMetaData2/3` /
  `SummaryInformation` / `DllDirectory` stream decoders. `.mb`
  IFF walker. `.ma` MEL tokenizer. JSON output schema documented.
  CLI binary `dcc-fileinfo` for ops use. Integration into the
  upload pipeline (ADR 0036's import framework + the existing
  direct-upload path) so the metadata is captured at ingest and
  written to the asset row. Tests against a corpus of real
  uploaded scenes.
- **1.47.B — Blender worker extension.** Add `io_scene_max` to
  the worker container build. Extend the existing worker dispatch
  to recognise `.max` + `.ma` extensions. Output is glTF written
  to the filestore at `assets/{id}/variants/scene.gltf` plus a
  rendered turntable preview. Cache invalidation hooks tied to
  the source file's content hash. Plugin-missing warnings flow
  back to the asset row from the Blender exit signal. An opt-in
  Dockerfile for the `mayapy`-enabled worker variant (`.mb`
  support) ships as documentation only — operators build their
  own.
- **1.47.C — `aa-dcc-viewer` premium add-on.** Separate repo
  (`mscrnt/aa-dcc-viewer`, private). Native Go parser for
  common-case `.max` chunks built clean-room from the Kaetemi
  spec, with `nrgsille76/io_scene_max` (Blender 4.2+, GPL-2.0+)
  used **only as a black-box oracle** — fed `.max` files in a
  sealed Blender container, glTF output captured for behavioural
  comparison, source never read by Layer 3 contributors.
  `io_scene_max`'s declared coverage (Editable Mesh / Poly,
  Standard / Physical / V-Ray / Corona / Arch & Design materials,
  bitmap refs, bones, cameras, lights) sets the high-water mark
  the native parser grows toward over time. Routing algorithm
  with chunk-ID heuristic database initialised from a curated
  test corpus. Material translation layer. HTTP interface
  exposing the `dcc-live-viewer` capability slot. Docker container
  distributed through the premium-add-on registry; Ed25519 `.lic`
  validated at startup. Clean-room contributor documentation in
  the private repo. **Trajectory**: native coverage starts at the
  common case (geometry / transforms / basic materials) with the
  Layer 2 Blender-glTF fallback carrying the rest; as the heuristic
  database accumulates and the native parser absorbs more chunk
  semantics, the Blender fallback path is invoked less often, and
  eventually only as a cross-validation oracle for chunks the
  native parser thinks it understands. Full-native is the asymptote,
  not the v1 deliverable — Blender stays in the loop as long as it
  earns its keep on edge cases.
- **1.47.D — Extended format coverage.** New sub-phase per
  format (`.hip`, `.spp`, `.zpr`). Same three-layer pattern.
  Demand-driven; no commitment to ship without customer pull.

ADR 0038's no-list is extended with one explicit clause: **no
premium add-on may incorporate GPL-licensed reverse-engineering
code**. The clean-room boundary is project-wide policy, not
specific to this ADR. A small follow-up edit lands ADR 0038 with
that clause.

## References

- [Kaetemi blog series on `.max` internals (parts 1–5)](https://blog.kaetemi.be/) — the only written spec; clean-room
  primary source.
- [Microsoft Compound File Binary format spec](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-cfb/) — the
  container `.max` lives in; public, supported.
- [Autodesk KB — `FileAssetMetaData` enumeration](https://help.autodesk.com/) — supported
  access to texture / XRef references.
- [Autodesk `.mb` partial spec](https://help.autodesk.com/) — IFF
  chunked structure documentation.
- [`nrgsille76/io_scene_max`](https://github.com/nrgsille76/io_scene_max) — GPL-2.0+ Blender extension; **used as Layer 2 inside the
  Blender worker, never read for Layer 3 contributors**.
- ADR 0008 — Storage architecture. Cached Blender glTF artifacts
  are storage-backed variants on the asset.
- ADR 0011 — Asset entity. `.max` / `.mb` / `.ma` files are
  assets; the layers attach to the same asset row.
- ADR 0028 — PBR 3D viewer polish. The browser-side viewer
  consuming Layer 3's output is the same Universal Asset Viewer
  arc.
- ADR 0034 — Capability add-ons. `aa-dcc-viewer` ships through
  this registry; `dcc-live-viewer` is a new capability slot.
- ADR 0036 — External imports. Imported `.max` files trigger the
  same three-layer pipeline; no new code path required.
- ADR 0038 — Premium add-on layer. The distribution / licensing /
  EULA / Ed25519 `.lic` infrastructure for Layer 3.
