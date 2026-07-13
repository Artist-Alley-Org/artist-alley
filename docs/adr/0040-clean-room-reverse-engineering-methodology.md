---
id: "0040"
title: Clean-room reverse-engineering methodology
status: accepted
date: 2026-05-31
area: process
phases: []
supersedes:
  - "0001"
related:
  - "0016"
  - "0034"
  - "0038"
  - "0039"
tags:
  - process
  - licensing
  - legal
  - reverse-engineering
  - conventions
excerpt: >-
  Project-wide procedure for producing demonstrably non-derivative
  implementations of publicly-specified or reverse-engineered
  functionality — three-phase Spec / Observation / Implementation
  flow, contributor quarantine, approved spec sources, and per-decision
  provenance documentation. Applies to any work that lands in a
  closed-source artifact or that we want to keep license-portable.
---
## Context

ADR 0038 introduced a premium add-on layer — closed-source,
EULA-licensed artifacts that ship on top of the AGPL core. Its
explicit no-list forbids any premium add-on from incorporating
**code derived from GPL / AGPL / LGPL reverse-engineering work**.
ADR 0039 (native DCC format viewers) applies that rule concretely:
the `aa-dcc-viewer` premium add-on must be built clean-room from
public specs and behavioural observation, never from existing GPL
implementations like `io_scene_max`.

The rule by itself is not enforceable. "Don't read the GPL source"
is easy to violate by accident, hard to audit after the fact, and
the legal value of a clean-room claim depends entirely on whether
we can **demonstrate** the procedure was followed. A contributor
who skims an open-source implementation "just to check the chunk
layout" has contaminated themselves and the project; a code review
that doesn't ask the right questions will let it through.

This is well-trodden ground. The legal pattern — clean-room
re-implementation of publicly-specified functionality — has decades
of precedent:

- **Phoenix Technologies** vs. IBM's BIOS (1984). Phoenix engineers
  who had never seen IBM's BIOS implemented the same interface from
  a behavioural spec written by a separate team. The result was
  ruled non-derivative. This case established the modern clean-room
  procedure.
- **Compaq's BIOS** followed the same pattern.
- **Sega v. Accolade** (1992) — reverse engineering for
  interoperability is fair use in the US.
- **Sony v. Connectix** (2000) — same principle extended to runtime
  emulation; clean-room methodology was load-bearing in the
  defence.
- **Wine, ReactOS, Samba, GNU Hurd, NewLib, dozens of compatibility
  libraries** — production examples of clean-room implementations
  of proprietary specifications shipping today.

The procedure is procedural, not magical. It works because it
breaks the chain of derivative-work claims at a documented point,
and because **provenance documentation** lets us reproduce the
break under audit.

This ADR codifies the procedure for Artist Alley so every future
reverse-engineering project (`.mb` Maya binary, `.hip` Houdini,
`.spp` Substance Painter, `.zpr` ZBrush, follow-on DCC formats,
any proprietary platform integration that requires format-level
work) can reference one canonical policy instead of re-deriving it.

## Decision

Adopt a **three-phase clean-room methodology** for any work that
either (a) lands in a closed-source / proprietary artifact, or
(b) we want to keep license-portable independent of how upstream
implementations are licensed. The methodology has documented
contributor-quarantine rules, an approved-source catalogue, and
per-decision provenance logging. Code review enforces it.

### When clean-room applies

Required:

1. **Any code in a premium add-on** (per ADR 0038) that performs
   reverse-engineering, format-parsing, or protocol-implementation
   work where an open-source implementation exists.
2. **Any in-tree code under the AGPL core** that we want to keep
   re-licensable (e.g. to move into a premium add-on later, or to
   dual-license).
3. **Any code that interoperates with a proprietary format whose
   only available implementations are GPL / AGPL / LGPL / SSPL /
   other copyleft.**

Not required:

1. **In-tree AGPL code we are happy to ship under AGPL forever.**
   We can read other AGPL implementations freely; we just commit
   that the work stays in the AGPL core.
2. **Subprocess invocations of GPL software** (the Blender worker
   pattern, the `mayapy` worker pattern). The GPL boundary
   terminates at the process boundary when data crosses as files,
   not as linked code. The subprocess pattern is the explicit
   safe-harbour for GPL helpers (NVIDIA kernel-module precedent
   on Linux; every commercial build chain calling GPL `make`).
3. **Upstream contributions back to a GPL project.** When we
   contribute improvements to `io_scene_max` or any other GPL
   project, we read its source freely — we're working under its
   license. The improvements stay GPL upstream.

### The three-phase procedure

**Phase 1 — Specification.**

Acceptable inputs to the specification phase:

- Format vendor's published documentation (Autodesk KB articles,
  Microsoft CFBF spec, Adobe published specs, etc.).
- Third-party narrative reverse-engineering write-ups — text that
  *describes* structure without being *code that implements* it
  (e.g. Kaetemi's `.max` blog series; the various IFF chunked-format
  papers).
- Standards documents (IEEE, ISO, RFC, W3C, Khronos).
- Behavioural observations from Phase 2 (see below).
- Existing implementations the project itself owns or has
  unambiguous license to (clean-room past output is a valid input
  to clean-room future work).

Not acceptable inputs to the specification phase:

- Source code of any GPL / AGPL / LGPL / SSPL / proprietary
  implementation of the same format / protocol / interface.
- Disassembled or decompiled output of any proprietary
  implementation.
- Internal documentation from the vendor obtained under NDA or
  through unauthorised disclosure.

The specification output is a written description, in our own
words, of *what* the format does — structure, semantics, edge cases.
Not code. The spec lives in the relevant project's repository under
`docs/spec/<format>/` and is itself reviewable.

**Phase 2 — Observation.**

Behavioural observation of black-box implementations is permitted
and is often the only way to fill gaps in published specs. Acceptable
observation methods:

- Run the format's authoring DCC (3ds Max, Maya, Houdini, etc.)
  on test files and observe inputs vs. outputs. We have legitimate
  licenses to the DCC.
- Run open-source implementations *as black boxes*: feed files in,
  capture outputs, work backward to behaviour. The open-source
  implementation runs in a sealed environment (a container, a
  subprocess) and only its *outputs* — not its source — enter the
  observation log.
- Diff outputs across implementations to identify semantically
  equivalent encodings.
- Fuzz tests against the authoring tool to probe edge cases.

What observation is **not**: reading the open-source implementation's
source to "check our understanding." That is source reading, not
observation, and contaminates the contributor.

Observation outputs feed back into Phase 1 — the spec is updated
with the observed behaviour, attributed to "observation of `<tool>`
on `<test corpus>`."

**Phase 3 — Implementation.**

Implementation contributors read **only** the Phase 1 spec
(including the observation-derived updates). They never read the
open-source implementation source. Their code expresses the spec
in our chosen language and architecture. The spec is the contract;
the implementation is the work.

If implementation reveals a gap in the spec (a case the spec
doesn't cover), the contributor opens an issue describing the gap
in our own terms. A spec-phase contributor — possibly a different
person — resolves the gap by extending the spec via more research
or observation. The implementation contributor then reads the
extended spec and continues. The gap-resolution loop must never
short-circuit through "let me just check what `io_scene_max` does
here."

### Contributor quarantine

Anyone who has read the source of a GPL / AGPL / LGPL implementation
of a format we are clean-rooming **is excluded from Phase 1
(specification) and Phase 3 (implementation) for that format**.
They may participate in Phase 2 (observation) and may write code
in unrelated areas of the project.

The quarantine lasts indefinitely. There is no "forget it" clean-up
path; memory of read source code is itself a contamination vector
under copyright law.

Anyone who *has not* read the source may participate in any phase.
The reading-log (below) is the evidence used to determine eligibility.

Contributors disclose their reading history at the time they
volunteer for a clean-room project. The disclosure is recorded in
the project's clean-room contributor register, separately from
public author attributions.

### Approved spec sources catalogue

Every clean-room project maintains an explicit catalogue of approved
spec sources under `docs/spec/<format>/SOURCES.md`. Each entry
includes:

- The source URL or citation.
- The license or copyright status (public domain, CC-BY,
  vendor-published, narrative write-up by named author).
- A one-line summary of what the source covers.
- The date the contributor accessed it (snapshot for archival).
- The contributor's name (acknowledgement, not gate).

A spec source that is **not** in `SOURCES.md` cannot be cited in
the spec. New sources are added by PR with review.

This catalogue serves two purposes: it scopes what contributors may
read during the project, and it gives an auditor a single
authoritative answer to "where did the spec come from?"

### Provenance logging

The specification document is line-cited. Every meaningful structural
claim ("chunk ID 0x0100 contains an array of vertex positions";
"the size field MSB indicates a child chunk follows"; "the
texture path is encoded as UTF-16 little-endian") links to:

- A specific section of a `SOURCES.md` entry, or
- A specific Phase-2 observation log entry (file, tool version,
  observation date, observation output captured).

A spec claim without a citation does not land. A spec claim citing
a non-approved source does not land. The provenance log is the
production-grade demonstration that the implementation is built
from non-derivative inputs only.

Implementation code does not need per-line citations to the spec
— the spec exists separately and the implementation is the
expression of it. But implementation-time discoveries (a chunk
encoding the spec didn't cover, an edge case that needed
behavioural verification) round-trip back into the spec with
updated citations.

### Convergent implementation (explicitly allowed)

Two clean-room implementations of the same spec will often look
structurally similar — same enum names, same loop shape, same error
codes, same dispatch table layout. This is **expected and not
infringement**. The format dictates a lot of the implementation
shape: any conformant parser walks the same chunk tree, recognises
the same IDs, handles the same edge cases. Convergent shape is
evidence the spec is good, not evidence of copying.

The line between convergent (legal) and derivative (not legal) is
the contributor's provenance, not the output's similarity. A
contributor who has never read the original cannot produce a
derivative work even if the result looks identical — they arrived
at it independently.

A challenger has to show the work *derived from* the original. The
provenance log + the quarantine register is the answer to that
challenge.

### The "clever fix" trap

A common failure mode: the implementation contributor hits an edge
case (file produces wrong output; unfamiliar chunk; opaque
sub-payload). The temptation is to "just check how `io_scene_max`
handles this." Doing so contaminates the contributor and ends
their participation in Phase 3 for this format.

Correct response: the contributor describes the problem in our own
terms ("file X produces wrong material color for chunk type Y"),
opens an issue, and a Phase-2 observation pass investigates the
behaviour against the authoring DCC and against the open-source
implementations *as black boxes* (input → output, no source). The
finding goes into the spec with a new citation. The implementation
contributor reads the updated spec and continues.

This loop is slower than reading the GPL source. That is the cost
of the methodology. The cost buys the legal posture.

### Code review for clean-room compliance

Every PR touching clean-room code answers two questions in the
description:

1. **Spec citation.** Which sections of the spec does this PR
   implement or extend? (Link to the spec doc and the relevant
   anchors.)
2. **Source disclosure.** Did the author read any open-source
   implementation of this format / protocol during this work?
   (Expected answer: no. Yes triggers immediate contributor-quarantine
   review.)

Reviewers verify the spec citations resolve to entries in
`SOURCES.md` and that the implementation matches the spec text
rather than visibly mirroring an upstream implementation's
structure. Reviewers also check that any spec gaps surfaced in the
PR have proper provenance trails — a new spec citation, not a
silent design choice.

### Subprocess boundary as the GPL safe-harbour

The Blender-worker pattern (ADR 0039 Layer 2) and the future
`mayapy`-worker pattern are GPL-compatible because:

- The GPL software runs in its own process. Linking does not occur
  across the process boundary.
- Data crosses the boundary as files (input `.max`, output glTF),
  not as linked code.
- This is the explicit pattern the FSF documents as compatible
  ("aggregation"). It is not a loophole; it is the intended
  composition mechanism.

Subprocess-bounded GPL invocations do **not** count as derivative
incorporation of the GPL software. Our core can call them freely;
our premium add-ons can call them freely; the GPL boundary stays on
the other side of the process.

Subprocess invocations do *not* exempt the calling code from clean-
room methodology if the *calling* code itself reverse-engineers the
proprietary format. The subprocess is one of several patterns in
the methodology, not a wholesale exemption.

### Upstream contribution pattern (explicit alternative)

When we *want* to contribute improvements back to a GPL project
(e.g. fix a bug in `io_scene_max`, add Substance Painter material
support to a Blender plugin), we work **under that project's
license**. Reading the source freely is fine because we are
producing GPL output that lives in the GPL upstream.

The improvements stay GPL. We benefit downstream by running the
improved upstream in our Layer-2 Blender worker. We do **not**
later pull those improvements (or any work derived from reading
them during contribution) into a premium add-on. Contributors who
do upstream work in a format space then become quarantined from
Phase 1 / Phase 3 work on that format in our codebase. This is the
intended boundary.

## Consequences

### Positive

- The "no GPL-derived code in premium add-ons" rule from ADR 0038
  becomes enforceable rather than aspirational. PR review has a
  concrete checklist; the provenance log produces a paper trail.
- A challenger asking "did you derive your `.max` parser from
  `io_scene_max`?" gets answered by the quarantine register + the
  spec's citation list + the observation logs. The defence is
  document-backed, not vibes-backed.
- Future format-specific ADRs (.mb / .hip / .spp / .zpr / etc.) can
  reference this ADR by single number instead of re-deriving the
  methodology each time.
- Contributors who *want* to contribute upstream to existing GPL
  projects have a clean path that doesn't trap them out of our
  clean-room work — the boundary is documented and they pick which
  side they're on per project.
- The methodology applies uniformly across the project, so adding
  a new clean-room target (e.g. a future proprietary plugin
  format) is a matter of starting a `SOURCES.md` and a contributor
  register, not re-inventing process.
- Reading discipline is a known-quantity engineering practice
  (Wine / ReactOS / Samba / Phoenix BIOS all live by this). We are
  not pioneering anything; we are adopting battle-tested workflow.

### Negative

- Real overhead. Maintaining `SOURCES.md`, the contributor
  register, per-claim provenance citations in the spec, and PR
  review questions adds friction to every clean-room PR. The cost
  is paid in engineer-hours.
- Phase 2 observation is genuinely slower than reading the GPL
  source. When an edge case appears, "go look at how the working
  implementation handles it" is the natural debugging move; we
  forbid it. The team has to absorb the slowdown.
- The contributor-quarantine rule means anyone who has previously
  read `io_scene_max` (or any other GPL `.max` parser) is permanently
  excluded from `aa-dcc-viewer` Phase 1 / Phase 3 work. We may
  lose useful contributors to projects whose format space they
  already know best — and they cannot un-know what they read.
- The methodology depends on contributor honesty about reading
  history. There is no technical enforcement of "did this person
  read the GPL source last year on a personal project?". We rely
  on disclosure at the time of joining a clean-room project.
- Convergent-implementation defence works in court, but a hostile
  challenger can still impose litigation cost even when they would
  ultimately lose. The methodology reduces but does not eliminate
  that exposure.
- Some specs simply do not exist publicly. For a format with no
  Kaetemi-equivalent narrative writeup, Phase 1 becomes "write the
  spec from observation alone," which is dramatically slower than
  starting from existing prose. This may make some formats
  effectively unviable as clean-room targets.

## Alternatives considered

- **No methodology — trust contributors.** Rejected because the
  ADR 0038 rule depends on demonstrability. "Trust me" is not a
  defence against a license-violation claim.

- **Avoid all GPL-adjacent work; ship Blender worker only,
  forever.** Rejected because Layer 3 of ADR 0039 (the native
  parser + routing algorithm) is the actual moat. Skipping it
  leaves us with a Blender-as-a-service product that competes on
  thinner ground.

- **License the existing implementation under separate terms from
  upstream.** Some GPL projects sell commercial licenses. Rejected
  for `io_scene_max` specifically because it is not commercially
  licensed; rejected as a general strategy because it makes our
  premium add-on revenue dependent on upstream licensing decisions
  we don't control.

- **AGPL-license the premium add-ons too.** Rejected because the
  commercial premise of ADR 0038 depends on a paid closed-source
  artifact. AGPL'd premium add-ons would be immediately forkable
  and the revenue model collapses.

- **Only-permissive-license helpers (MIT / BSD / Apache).** Rejected
  because for some formats, the only available implementations are
  copyleft — there is no permissive `.max` parser. Restricting
  ourselves to permissive helpers cuts off coverage we need.

- **Outsource clean-room work to contractors who handle their own
  reading discipline.** Considered. Useful for specific targeted
  efforts. Rejected as the default because we still have to verify
  the contractor's process and maintain the provenance trail, which
  is most of the cost anyway.

## Implementation

Per-project boilerplate, applied to every clean-room project:

1. **Create `docs/spec/<format>/` in the relevant repository.**
   For Layer 3 work, this lives in the private premium-add-on
   repository.
2. **Initialise `SOURCES.md`** with the approved spec sources.
   Every source has license / copyright / coverage / access-date
   notes.
3. **Initialise the contributor register** as `CONTRIBUTORS.md` in
   the same directory. Each contributor's entry records reading
   disclosures and which phases they're cleared for.
4. **Adopt the PR template** with the spec-citation and
   source-disclosure questions.
5. **Add review responsibility** — a designated clean-room reviewer
   for the project who verifies the citation trail. Can be a
   project lead; not a separate ceremony role.
6. **Audit cadence** — quarterly review of `SOURCES.md` +
   `CONTRIBUTORS.md` + a sample of recent PRs against the rules.
   Catches drift before it accumulates.

Project-wide artifacts that ship alongside this ADR:

- A reference `SOURCES.md` template and `CONTRIBUTORS.md` template
  in `docs/templates/clean-room/`.
- A PR-template stanza for repositories with clean-room work.
- The clean-room-eligible labels (`clean-room:phase-1-spec`,
  `clean-room:phase-2-observation`, `clean-room:phase-3-implementation`)
  on the relevant repositories.

These templates land alongside this ADR; project-specific
adoption happens per-phase in ADR 0039 (Phase 1.47.C) and any
future clean-room phase.

## References

- [Phoenix Technologies clean-room BIOS history](https://en.wikipedia.org/wiki/Clean_room_design)
  — Wikipedia summary of the modern clean-room procedure's
  founding precedent.
- [Sega v. Accolade (1992)](https://en.wikipedia.org/wiki/Sega_v._Accolade)
  — reverse engineering for interoperability is fair use.
- [Sony v. Connectix (2000)](https://en.wikipedia.org/wiki/Sony_Computer_Entertainment,_Inc._v._Connectix_Corp.)
  — clean-room methodology in runtime emulation.
- [GNU GPL FAQ — Aggregation](https://www.gnu.org/licenses/gpl-faq.html#MereAggregation)
  — the FSF's documented position on subprocess-boundary safe-harbour.
- [Kaetemi `.max` blog series](https://blog.kaetemi.be/)
  — the canonical Phase-1 source for `.max` format work.
- [Wine project's clean-room policy](https://wiki.winehq.org/Clean_Room_Guidelines)
  — production reference for the procedure at scale.
- [ReactOS clean-room policy](https://reactos.org/wiki/Clean_Room_Reverse_Engineering)
  — same.
- ADR 0016 — License direction. The AGPL + commercial dual posture
  this methodology preserves.
- ADR 0034 — Capability add-ons. The architectural layer where
  clean-room artifacts ship.
- ADR 0038 — Premium add-on layer. The explicit rule this ADR
  makes enforceable.
- ADR 0039 — Native viewers for proprietary DCC formats. The first
  load-bearing application of this methodology.
