---
id: "0076"
title: One time-based media player, with frame-scoped review annotation
status: accepted
date: 2026-07-29
area: ux
phases: []
supersedes: []
related:
  - "0012"
  - "0037"
  - "0053"
tags:
  - viewer
  - video
  - audio
  - annotation
  - review
excerpt: >-
  Video and audio already share one player component; this records that
  decision and extends it. The review surface addresses time in FRAMES, not
  seconds, annotations are scoped to a frame rather than floating over the
  asset, the scrubber renders which frames carry annotations, and adjacent
  frames' strokes ghost behind the current one at a configurable opacity.
---

# One time-based media player, with frame-scoped review annotation

## Context

`MediaView.svelte` already serves **both video and audio** from one component.
Its transport — play, pause, seek, step, rate — is installed onto the viewer
controller once, and only the visual surface diverges: a `video` element
renders frames, while audio renders a waveform with a progress mask. Both use
a single `mediaEl: HTMLMediaElement`, so the scrubber, the hotkeys and the
transport bar are wired off the controller rather than off the element kind.

That unification was made in code and never written down. It is load-bearing
enough to deserve a record: the next person adding a time-based media kind
(image sequences, animatics, multi-track audio) needs to know the player is
one component and that adding a second one is a regression, not a feature.

The second half of this decision is new. The product's stated backbone is the
viewer, and the highest-value thing a studio does in a viewer is **review
moving pictures together**. Today the pieces exist but do not meet:

- A working brush engine (`BrushCanvas`, `brushes.ts`) built for the
  whiteboard.
- Comments on assets and posts.
- A frame-accurate transport.

What is missing is the join. Verified: nothing in the codebase scopes an
annotation to a frame — `frame_number` / `frameNumber` appear nowhere. A
drawing made while reviewing is not attached to the moment it describes, and
a comment about frame 178 is a comment about the asset that happens to
mention a number.

## Decision

**One player component serves all time-based media, and review annotation is
frame-scoped.**

### 1. The frame is the unit of address — already true, recorded here

Time-based review is conducted in **frames, not seconds**. The player
already does this: the controller carries `detectedFps`, the HUD renders
`HH:MM:SS:FF`, stepping moves ±1 / ±10 frames, and audio runs on a
synthetic 1000 fps rate (1 ms per "frame") so a mark in a score behaves
identically to a mark in a shot. This section records that model rather
than proposing it. Seconds are a
display convenience; the frame is what a reviewer names, steps to, and draws
on. Consequences:

- The transport steps by frame, and the displayed position is a frame index
  alongside a timecode.
- Every review artifact — an annotation, a comment, a marker — records a
  frame index, and the frame rate needed to interpret it.
- Assets whose frame rate is unknown or variable degrade to a single-frame
  model rather than guessing. **Guessing a frame rate silently misplaces
  every annotation on the asset**, which is worse than declining to offer
  frame addressing.

### 2. Annotations belong to a frame, not to the asset

An annotation is a stroke set plus the frame it was drawn on. It renders only
while that frame is displayed. This is what makes drawing on moving media
meaningful: a circle around a character's hand means *this* hand position, not
"somewhere in this shot".

Annotations reuse the **existing brush engine**. A second drawing
implementation for review would drift from the whiteboard's within a release
— same class of duplication this ADR forbids for the player itself.

### 3. The scrubber shows where the annotations are

The timeline renders a marker per annotated frame, positioned by frame index.
This is the feature that turns a review recording into a review *document* —
a reviewer scrubs to the marks rather than hunting for them, and the density
of marks along the bar is itself the summary of where attention was spent.

The scrubber therefore carries three layers: the media's own shape (waveform
for audio, or a thumbnail strip), annotation markers, and the playhead.

### 4. Adjacent frames ghost behind the current one

Strokes on nearby frames render behind the current frame's at reduced
opacity, at a level the reviewer chooses (including off). Animation notes are
rarely about one frame in isolation — an arc drawn across six frames is only
legible when the neighbouring marks are visible.

This is a **view** setting, not a property of the annotation. It changes what
you see, never what is stored.

### 5. Controls stack rather than crowd

The surface carries three distinct control families and they get distinct
rows rather than one overloaded strip:

- **Transport** — play, step, rate, loop range, frame position.
- **Drawing** — tool, colour, size, opacity, undo.
- **View** — fit, flip, grid/guide overlay, ghosting level, comparison.

Rows collapse by priority on narrow viewports; transport is the row that
survives longest, because a reviewer who cannot draw can still watch.

### 6. Comparison is a mode of the same player

Comparing two versions side by side, or wiped, is the same player instantiated
twice against one transport — not a separate component. Their playheads are
locked by default so a difference at a frame is a difference at the same frame.

## What this explicitly rejects

- **A separate video player and audio player.** Already avoided; recorded here
  so it stays avoided.
- **Second-based annotation anchoring.** Floating-point seconds do not survive
  a frame-rate change and cannot express "the frame after this one".
- **A review-only drawing engine.** Reuse the brush engine.
- **Storing ghosting/opacity on the annotation.** It is a view preference.

## Consequences

- Annotations gain a frame index and a frame-rate reference, so the schema
  must carry both. An annotation without its rate is unplaceable.
- The player becomes the most complex component in the frontend, which is the
  argument for it being exactly one component rather than several.
- Variable-frame-rate media needs an explicit answer before it can be
  annotated; until then it degrades honestly rather than misplacing marks.
- The scrubber does real work now — media shape, markers, playhead — and
  needs to stay responsive while scrubbing a long asset. Marker rendering is
  bounded by frame count, not annotation count.
- Federation: a frame-scoped annotation is meaningful on a peer only if the
  asset's frame rate travels with it (ADR 0037 already treats caption tracks
  as portable artifacts; annotations follow the same reasoning).

## References

- ADR 0012 — metadata model (custom fields, audit history)
- ADR 0037 — caption and subtitle artifacts, the existing timeline-anchored artifact
- ADR 0053 — IIIF interoperability, for how still frames are addressed
