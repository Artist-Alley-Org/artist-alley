---
id: "0078"
title: 3D animation clips are addressable independently of their mesh, and rig identity is extracted
status: accepted
date: 2026-07-29
area: architecture
phases: []
supersedes: []
related:
  - "0028"
  - "0039"
  - "0069"
  - "0076"
tags:
  - 3d
  - animation
  - viewer
  - metadata
excerpt: >-
  The viewer can already play animation clips, but only the ones baked into the
  file it loaded. Making a shared animation library possible needs two things
  recorded: a clip is addressable independently of the mesh that shipped it, and
  a rig's identity is an extracted property rather than something inferred at
  view time. Shared-rig playback is the supported path; cross-rig retargeting is
  explicitly not promised.
---

# 3D animation clips are addressable independently of their mesh, and rig identity is extracted

## Context

The 3D viewer already plays animation. `ModelView.svelte` builds an
`AnimationMixer` over the loaded model and selects clips by index
(`mixer.clipAction(rawAnimations[idx])`), with play/pause and speed wired
to the viewer session. What it cannot do is play a clip that did not
arrive inside the file being viewed: `rawAnimations` is populated solely
from the loader result — `result.animations` for glTF/GLB, the FBX
loader's own `animations` array.

That limitation is a data-model gap, not a rendering one, and it surfaced
as a concrete request from an animation team: a set of characters that
deliberately **share one rig**, plus a pool of animations authored once
and applied across all of them. The reference point people reach for is
a game-asset web viewer where you pick a model, then pick any animation
and watch it play on that model.

Three facts shape the decision.

**The mixer does not care where a clip came from.** `AnimationMixer`
binds a clip's tracks to objects by **name**. If the clip's track names
match the target's bone names, it plays — the clip's provenance is
irrelevant. So for meshes that genuinely share a rig, "apply any
animation to any mesh" needs no transformation whatsoever.

**Cross-rig retargeting is a different problem, and three.js is weak at
it.** `SkeletonUtils.retargetClip` exists but is used in no official
example and carries standing correctness bugs — results off by one frame,
errors when passed its own documented parameter types, and visibly wrong
output (inverted feet, reversed hands) on common rigs. Treating
retargeting as an available library call would be a promise the platform
cannot keep.

**Nothing today knows which assets share a rig.** Bone names are not
extracted, clip names are not extracted, and no relation records rig
compatibility. Answering "which animations can play on this mesh?"
currently requires opening and parsing every candidate file.

No prior ADR covers 3D animation at all. ADR 0028 governs PBR inspection
(materials, wireframe, UV); ADR 0039 covers native DCC formats; ADR 0069
covers preview rendering. The gap is this one.

## Decision

**An animation clip is addressable independently of the mesh that shipped
it, rig identity is an extracted property, and shared-rig playback is the
supported capability.**

### 1. A clip is a first-class thing, not a property of one asset

An animation clip is identified by the asset that carries it plus its
name and index. That identifier is what a playlist, a comment, or a
library entry refers to. The clip does not have to live in the mesh being
viewed, and an animation-only file — an FBX exported with a skeleton and
no renderable geometry — is a legitimate asset rather than a degenerate
one.

### 2. Rig identity is extracted at ingest, not inferred at view time

Ingest records, for any asset containing a skeleton or clips:

- the **rig signature** — a stable hash over the sorted bone-name list,
- the **bone names** themselves, so a near-miss can be diagnosed rather
  than merely failing,
- the **clip inventory** — name, duration, and track count per clip.

Two assets with equal rig signatures are rig-compatible by construction.
This is what turns "which animations fit this mesh?" into a metadata
query instead of a parse of the whole catalogue.

Extraction happens at ingest **because the alternative is reprocessing
the entire 3D catalogue later.** The cost of adding it now is one
extractor; the cost of adding it after the catalogue grows is a migration
plus a backfill job.

### 3. Shared-rig playback is supported; retargeting is not promised

The supported capability is: given a mesh and a clip whose rig signature
matches, play it. That is a name-binding operation the mixer already
performs, so it is cheap and its failure mode is honest — signatures
either match or they do not.

**Cross-rig retargeting is explicitly out of the supported set** until a
spike proves a path. If it ships, it ships as a distinct, clearly-labelled
capability with its own quality bar — never as a silent fallback when
signatures do not match. A silent retarget that produces inverted feet is
worse than a refusal, because it looks like the artist's animation is
broken.

Where signatures do not match, the surface says so and names the
mismatching bones. Degrading honestly beats guessing.

### 4. A playing 3D animation is time-based media — reuse ADR 0076

A 3D clip has a duration, a playhead, a frame rate and a transport. That
is the same shape ADR 0076 already decided for video and audio, including
its central rule that **review artifacts are scoped to a frame**.

So the 3D animation surface **reuses ADR 0076's transport and
frame-addressing model rather than building a second one**. A note on
frame 42 of a walk cycle is the same kind of object as a note on frame 42
of a shot. ADR 0076 forbids a second player for a new time-based kind;
this ADR is that rule being applied rather than an exception to it.

The frame-rate caveat from ADR 0076 §7 applies with a twist in our
favour: a 3D clip is sampled from keyframe data we control, not decoded
from a compressed stream, so the seek-accuracy problem is materially
easier here than it is for HLS video.

## What this explicitly rejects

- **"Apply any animation to any rig."** Not deliverable on the current
  toolchain, and offering it produces silently wrong output.
- **Retargeting as an implicit fallback.** If signatures do not match, the
  answer is a refusal with a reason, not a guess.
- **Clips reachable only through their own asset's viewer.** That is the
  present limitation and the thing this ADR removes.
- **Inferring rig compatibility at view time** by parsing candidates. It
  does not scale and it makes the library a per-request cost.
- **A separate transport, scrubber, or annotation model for 3D.** ADR 0076
  owns that; adding a second one is the duplication both ADRs forbid.

## Consequences

- The 3D extractor grows a skeleton/clip pass, and assets gain rig
  metadata. Existing 3D assets need a backfill — bounded, and much smaller
  now than it will ever be again.
- An animation-only file must survive the pipeline as a first-class asset
  even though it renders no meaningful thumbnail. Preview generation has to
  tolerate "nothing to look at" without marking the asset failed.
- Rig signature is a **name-based** identity. It will report two rigs as
  compatible when bone names match but rest poses differ, which can play
  distorted. Signature equality is a necessary condition, not a sufficient
  one, and the surface should not claim more than it verified.
- Federation: a clip reference travelling to a peer is meaningful only if
  the rig signature travels with it, the same portability requirement ADR
  0076 notes for frame rates and ADR 0037 for caption tracks.
- The viewer gains a second asset source at render time — it must load
  clips from an asset other than the one on screen, which touches
  capability checks. **Reading a clip is reading content**, so it goes
  through `visibility.CanReadContent` like any other byte read, per
  ADR 0063/0064. A shared animation library must not become a way to read
  bytes from an asset the caller cannot otherwise see.

## References

- ADR 0028 — PBR 3D viewer polish (materials/UV inspection; no animation scope)
- ADR 0039 — native DCC format viewers
- ADR 0069 — preview rendering via headless three.js
- ADR 0076 — one time-based media player, frame-scoped review annotation
