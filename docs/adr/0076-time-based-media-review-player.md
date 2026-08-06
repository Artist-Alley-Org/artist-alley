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
  - "0078"
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

*Amended 2026-07-29:* **3D animation clips are one of those kinds.** ADR 0078
applies this decision to the 3D viewer — a playing clip has a duration, a
playhead and a transport, so it reuses the model below rather than growing a
parallel one. Its frame accuracy is *easier* than video's, because a clip is
sampled from keyframe data we hold rather than decoded from an HLS stream.

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

Markers are also a **navigation target**: jump-to-previous / jump-to-next
annotation belongs in the transport. On a long asset with sparse notes,
scrubbing to find the next mark is the slow path, and a reviewer working
through feedback moves mark-to-mark rather than second-to-second.

The same addressing serves **comments**, not only strokes: a comment
carrying a frame index is a navigable review artifact, and selecting it
moves the playhead to that frame. This is why §1 specifies that *every*
review artifact records a frame, not just annotations.

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

### 7. Frame accuracy is not free, and the browser does not guarantee it

This is the risk that can invalidate everything above, so it is stated as a
decision rather than left as an implementation detail.

The HTML media element **does not guarantee frame-accurate seeking**. Internal
rounding can land on the end of the previous frame rather than the start of
the requested one. `requestVideoFrameCallback` — which the player already uses
— runs on the main thread while compositing happens on the compositor thread,
so it is explicitly best-effort with no strict guarantee. Safari does not
implement it at all, and seeks to the nearest keyframe more aggressively than
Chromium. Seek cost also varies sharply with distance from a keyframe, which
matters because our video ships as an HLS ladder with keyframe intervals.

**A frame-scoped annotation is only as trustworthy as the seek that lands on
that frame.** If a reviewer draws on frame 178 and a later seek to 178 renders
177, the annotation is silently on the wrong picture — a failure that looks
like sloppy drawing rather than a bug, which makes it worse.

Therefore:

- The frame a stroke is stored against is the frame the player **believes it
  displayed**, captured at draw time — never re-derived later from a
  timestamp.
- Round-tripping is a correctness requirement, not a nicety: seek to a stored
  annotation's frame and the same picture must appear. This must be tested
  against real media, on more than one browser engine.
- Where the platform cannot deliver the guarantee, prefer degrading the
  addressing (§1's trustworthiness gate) over presenting a frame number that
  is confidently wrong.
- WebCodecs is the escape hatch if element-based seeking proves insufficient.
  Not adopted now — it is a much larger commitment — but the annotation model
  above is deliberately independent of *how* a frame is produced, so switching
  the decode path later does not invalidate stored annotations.

### Amendment 2026-08-05 — the mechanisms §7 asked for, from a working implementation

§7 states the frame-accuracy risk and says to "degrade the addressing rather than present a
frame number that is confidently wrong" — without saying how a caller would *know*. A
prior-art pass over a shipped review player (a private codebase; patterns only, no code) supplied
the missing mechanisms. Each is recorded as a decision, because §7 is unimplementable without
them.

**1. The player is the sole authority on frame rate, and the rate is resolved server-side.**
Probe it during ingest (we already run ffprobe in the media pipeline) and publish it to the
client. Browser-side detection is a **fallback only**: `requestVideoFrameCallback` cannot
reliably distinguish 29.97 from 30 on a short clip, which is precisely the error that puts a
stroke on the wrong picture. Never assume 30.

**2. Frame data carries the PROVENANCE of the rate that produced it**, not just the rate.
Expose where the number came from — probed / declared in metadata / detected in-browser /
defaulted — and treat anything written under "defaulted" as untrustworthy. This is the concrete
form of §7's trustworthiness gate: a UI can refuse to write a frame-scoped annotation, or mark it
provisional, instead of silently recording a guess. The Consequences below already require an
annotation to carry its rate; this adds that the rate must carry its own confidence.

**3. Seek to the MIDDLE of a frame's display interval, never to its boundary.** Boundary
seeking is subject to float error and decodes the neighbouring frame. This is the single most
load-bearing implementation detail for §7's round-trip requirement, and it is not discoverable
from the spec — only from having been burned by it.

**4. One funnel for every playback mutation.** Play, pause, seek, step, rate and in/out range all
go through a single code path that applies clamping, range limits and lock checks. Anything
touching the media element directly bypasses the gating. Same argument as ADR 0063 for the
visibility predicate: one enforcement point, or the rule is advisory.

**5. Playback locks are NAMED and released by their owner.** A feature that needs the playhead
frozen — drawing is the obvious one — claims a lock under its own name; playback resumes only
when every claim is released. A shared boolean lets two consumers clobber each other, and this
player will have at least three (annotation, comparison, presentation). When an action is
refused, emit an event carrying the owners responsible, so the UI can say *why* rather than
appearing dead.

**6. Remote-originated changes are marked, so they are not re-broadcast.** Without an explicit
"this came from a peer" flag, a synced session feeds back on itself.

### Amendment 2026-08-05 — where we go FURTHER than the prior art

The mechanisms above are adopted because they are correct. These four are places the reference
design is weaker than what we can build, and the reasons are ours, not inherited.

**1. Make a bad frame reference DETECTABLE, not merely avoided.** §7 and the rejects section stop
at "capture the frame at draw time." That prevents the *known* failure; it does nothing when the
rate itself was wrong — a container mis-declaring 30 for 29.97, later corrected by a better probe.
So a frame reference is stored **over-determined**: the frame index, the rate and its provenance,
*and* the media time the player believed it displayed. Any two of those predict the third, so a
disagreement beyond one frame is a **detectable** corrupt reference rather than a silent
misplacement. The reference implementation's ±1-frame search exists precisely because it could not
tell "close enough" from "wrong"; storing the redundancy is what buys that distinction. Cheap —
three scalars on a row we are already writing.

**2. Leases, not locks.** Named locks released by their owner (mechanism 5) have no answer for an
owner that never releases: a crashed tab, a closed laptop, a dropped connection freezes the
playhead for everyone with no recovery but a reload. A claim is therefore a **lease with an
expiry, renewed while the holder is alive**, and it lapses on its own. Same ergonomics, no wedged
sessions. This matters more for us than for the reference because our sessions are multi-user by
design.

**3. There is no unauthenticated relay to reason about.** The reference is careful that its relay
performs no authorisation and that possession of a socket proves nothing — correct, and forced on
it by the relay being a separate service. Because our stream is an endpoint in the same binary,
behind the same session and capability middleware as every other read, that caveat does not need
handling: it stops existing. The consequence worth naming is **revocation** — access lost
mid-session must stop the event flow, which a detached relay holding an opened socket cannot do
and we can. Every event is subject to the same visibility rule as the asset it concerns.

**4. One real-time substrate, not two.** ⭐ The reference synchronises strokes with a bespoke
protocol: an incremental instruction stream plus a full state broadcast every ~20 instructions.
That converges only approximately — the periodic full state exists to paper over whatever the
diff stream lost — and it is a second sync protocol to own.

But **a review annotation is the whiteboard's problem**: strokes on a canvas, edited live by
several people. The whiteboard arc already commits to **Yjs CRDT collaboration with an awareness
protocol** (Phase 1.20). Annotations must ride that same substrate rather than grow a parallel
one, for the same reason this ADR forbids a second drawing engine — two sync protocols diverge
within a release, and the one used less often is the one that rots.

CRDT convergence also removes the machinery the bespoke design needs: no periodic full-state
broadcast, no sequence-gap replay path, late joiners and reconnecting clients converge by
construction, and presence cursors come from awareness rather than a hand-rolled participant list.

**The split that makes this work — and it is a real distinction, not a hedge:**

| | substrate | why |
|---|---|---|
| **Annotation state** — strokes, layers, visibility | **Yjs document**, shared with the whiteboard | convergent, replayable, offline-tolerant; it is *state* |
| **Playback commands** — play/pause/seek/rate/range | **plain SSE events + POST** | ephemeral; a seek that arrives late must be *dropped*, never merged |

Putting playback commands in a CRDT would be a category error: converging on a stale seek is
exactly the "straggler yanks every follower backwards" failure, and a command's value expires.
The reference routes both down one channel; separating them is the improvement.

### Amendment 2026-08-05 — the shape of a synced review session (#5)

Recorded here rather than in a new ADR because it constrains this player's API surface. The
transport decision is the operator's (2026-08-05): **Server-Sent Events plus ordinary POSTs, not
a WebSocket framework** — and in our single Go binary, not a separate service (see the target
architecture; we do not ship sidecars).

- **Carry commands, never media.** Every client plays its own copy; the session relays
  *play / pause / seek-to-frame / rate / range*. This is why synced review stays frame-accurate
  where screen-sharing cannot: quality is bounded by each viewer's own file, not the presenter's
  uplink.
- **The transport performs NO authorisation.** Session membership is minted by the API, which
  enforces identity and per-asset read access. Possession of a live connection is never proof of
  permission — the same posture ADR 0063 takes for reads.
- **The transport holds no durable state.** Room membership is in-memory and disposable; the
  database remains the system of record. A refresh must not destroy a room, so an empty room gets
  a grace period before it is reaped.
- **Drop stale commands.** A delayed packet that arrives after its moment must be discarded, or a
  straggler yanks every follower backwards.
- **Features claim NAMED CHANNELS on the one connection, and payloads are forwarded verbatim.**
  The relay must not inspect, validate, store, or keep an allowlist of event names — otherwise
  every new review feature requires editing shared server code, coupling unrelated features
  together. Unknown inbound channels are ignored, which is also what makes version skew between
  peers survivable (relevant once sessions federate).
- **Session roles**: a presenter drives; attendees may *leave sync and stay in the room* rather
  than only leave; presenter control can be requested and handed over. "Follow" is a per-attendee
  state, not a property of the room.

### Amendment 2026-08-05 — two things the annotation model was missing (#6)

- **Strokes record the canvas dimensions they were drawn at.** Without them, an annotation drawn
  over a 720p proxy replays misaligned over a 4K original. Store the drawing surface's size with
  the stroke set and rescale on replay; do not assume the asset's native dimensions.
- **Layers.** A review annotation is not one stroke set but an ordered stack of named,
  individually visible layers. It is how one reviewer's marks stay separable from another's, and
  how a note can be toggled off without being deleted. §4's ghosting is a *view* of adjacent
  frames; layers are a *structure* within one frame. They compose, and both are needed.

  **A layer belongs to an author, not to a slot.** The reference caps layers at a fixed 4, which
  approximates the real requirement — keeping one reviewer's marks separable from another's — with
  a magic number that a fifth reviewer breaks. Layers are therefore **author-scoped by default**:
  joining a review gives you your own, and "hide everyone but Priya" is a filter over authorship
  rather than a hunt through a numbered stack. Additional layers within your own marks stay
  available for people who want them.

  Keep a bound, but bound the thing that actually costs: **layers rendered per frame**, not layers
  stored. Compositing is paid on every frame of playback; storage is not.

- **Session permissions use the ACL model we already have.** The reference carries an `acl_json`
  blob on the session row. We have a typed ACL substrate — principal type, permission, and
  `expires_at` — already enforcing post and collection sharing. A JSON permission blob on a new
  table would be a **second expression of the access rule**, which is the defect class epic #665
  exists for and which #892 and #904 each spent a sprint deleting. A review session is a shareable
  object; it gets the same ACLs as the other shareable objects, and it inherits expiry for free —
  which is exactly right for a review that should stop granting access when it is over.

**Strokes travel as instructions, not pixels.** The live path shares vector draw commands and
re-renders them locally; it never ships a raster. That is what makes a shared annotation
resolution-independent (see the canvas-dimensions point above) and cheap enough to send while a
stroke is still being drawn.

This constrains the annotation *format*, which is why it lives here rather than in #5: an
instruction list must be replayable **out of order** and **at a different canvas size** than it
was authored at. Out-of-order replayability is not an extra requirement bolted on for the network
— it is what makes the CRDT substrate above viable, and a format that only replays in sequence
would force the bespoke protocol back on us.

**Persist on stroke end, throttle during.** A long stroke must not issue a write per sample.

Note what the CRDT choice deletes from this list: no periodic full-state broadcast, no
sequence-gap detection, no catch-up path for late joiners. Those are all workarounds for a
transport that can lose a message, and they stop being needed when the format converges.

## What this explicitly rejects

- **A separate video player and audio player.** Already avoided; recorded here
  so it stays avoided.
- **Second-based annotation anchoring.** Floating-point seconds do not survive
  a frame-rate change and cannot express "the frame after this one".
- **A review-only drawing engine.** Reuse the brush engine.
- **Storing ghosting/opacity on the annotation.** It is a view preference.
- **Re-deriving a stroke's frame from a timestamp after the fact.** Seek
  rounding makes that lossy; capture the frame at draw time.

  **Confirmed empirically 2026-08-05.** A shipped review implementation does exactly this — it
  persists a seconds value and recovers the frame later as `round(seconds × fps)`. The cost is
  visible in its own source as tolerance constants: matching an annotation to a frame needs a
  **±1 frame** window plus a **0.05 s** time window, because the round-trip does not land where it
  started. A ±1-frame match means a stroke can render on the neighbouring picture, which is the
  precise failure §7 describes — and it degrades silently, looking like sloppy drawing rather than
  a bug. The same codebase's newer integration guidance tells consumers to store the integer frame
  number and the rate instead, so the author reached this conclusion independently. **Our rejection
  stands, now with evidence rather than reasoning: the fudge factor IS the cost of the design.**

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
