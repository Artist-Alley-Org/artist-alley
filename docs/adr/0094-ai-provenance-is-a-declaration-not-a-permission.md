---
id: "0094"
title: AI provenance is a declaration, not a permission — three states, and extraction can only ever corroborate
status: accepted
date: 2026-08-20
area: architecture
phases: []
supersedes: []
related:
  - "0090"
  - "0084"
  - "0081"
  - "0012"
tags:
  - metadata
  - provenance
  - upload
excerpt: >-
  An AI declaration is the maker's statement, not a permission on the work. Three states
  plus undeclared rather than a boolean, because assisted and generated are the distinction
  people argue about and NULL must never be mistaken for a disclaimer nobody made.
  Extraction may corroborate AI and can never establish its absence.
---

## Context

The owner asked for a per-work AI declaration at upload — *"Medium, subject matter, software
used, tags, mature content, and CreatedWithAI or NoAI"* — with the same friction budget as the
mature checkbox that shipped in v0.10.0 (#1167).

No ADR governed it. ADR 0026 is *AI creative editing* — the feature, parked at Phase 1.20+ — not
provenance. So #1167 was about to follow a *pattern* (the mature axis) rather than implement a
*decision*, and the pattern does not answer the questions that decide the column's shape.

A prior-art pass established the following, all verified rather than assumed.

**The DAM tradition has nothing to say here.** The reference DAM's schema carries no table or
concept for AI provenance, synthetic media, or content credentials. This is genuinely new ground
for the category, and the prior art lives in the metadata standards, not in DAM products.

**IPTC has a real, mature vocabulary — and it cannot express what we were asked for.**
`Iptc4xmpExt:DigitalSourceType` (namespace `http://iptc.org/std/Iptc4xmpExt/2008-02-29/`,
cardinality **0..1**, value a URI from a controlled vocabulary) carries ~17 terms spanning
AI-generated (`trainedAlgorithmicMedia`, `algorithmicMedia`), AI-composite
(`compositeWithTrainedAlgorithmicMedia` — *"augmentation, correction or enhancement using a
Generative AI model"*, `compositeSynthetic`) and non-AI production methods (`digitalCapture`,
`digitalCreation`, `humanEdits`, …). Version 2025.1 adds further AI properties (AI System Used, AI
Prompt Information).

⭐ **There is no term meaning "no AI was used", and absence of an AI term is not a signal.** The
vocabulary describes *how a thing was made*, not *whether the maker disclaims AI*. Confirmed from
both the NewsCodes vocabulary and the specification.

**Our own extraction reaches less far than it looks.** `Iptc4xmpExt` is an **XMP** schema, so our
IPTC extractor can never carry it — that extractor decodes legacy **IIM** datasets (record 2:
ObjectName, Keywords, Byline, Credit, CopyrightNotice, …) from JPEG only. The XMP extractor is the
only possible carrier, and it currently absorbs two namespaces (`dc`, `xmpRights`) across JPEG and
PNG. Nothing in the tree references `DigitalSourceType` or C2PA today.

**And extraction covers a fraction of our catalogue.** Every registered extractor (`exif`, `iptc`,
`xmp`) supports still images only. A 3D model, a video, an audio file or a document can never
produce an extracted provenance signal, and those are ordinary content here.

## Decision

**1. The declaration is the artist's, and it is stored in its own column — not as
`DigitalSourceType`.** The two answer different questions. IPTC asks *"what process produced
this?"*; we are asking *"does the maker declare AI involvement?"*. Because IPTC cannot express
"no AI", a declaration of **none** is unrepresentable there, and the field the owner asked for
would be silently lossy if we stored it that way.

**2. Three declared states plus undeclared, NOT a boolean.**

| value | meaning |
|---|---|
| `none` | the maker declares no generative AI was involved |
| `assisted` | generative AI was used in part — upscaling, inpainting, an AI-generated texture on hand-made geometry |
| `generated` | the work is substantially AI-generated |
| *NULL* | **undeclared** — nobody was asked |

A boolean was the obvious shape and it is the wrong one, for two reasons that only appear later:

- **The assisted/generated distinction is the one people actually argue about.** IPTC found it
  necessary enough to separate `compositeWithTrainedAlgorithmicMedia` from
  `trainedAlgorithmicMedia`, and "I upscaled a texture" versus "a model made this" are different
  claims about authorship. A boolean collapses them irreversibly, and widening it later cannot
  recover which of the two a `true` meant.
- **NULL is load-bearing and a boolean cannot hold it.** Every asset that exists today predates
  the question. With `NOT NULL DEFAULT false` those rows would assert *"the maker declares no AI"*
  on the maker's behalf — a fabricated disclosure, on a topic where a false disclaimer is the worst
  possible error. Undeclared must be distinguishable from declared-none forever.

**3. ⭐ Extraction may corroborate AI, and may never establish its absence.** This asymmetry falls
directly out of the vocabulary and is the rule the implementation hangs on:

- An extracted `trainedAlgorithmicMedia` / `compositeWithTrainedAlgorithmicMedia` /
  `compositeSynthetic` / `algorithmicMedia` is a **positive** signal and may **prefill** an
  *undeclared* work.
- **No extracted value, and no non-AI value, may ever set `none`.** Absence is not evidence, and
  `digitalCapture` means "it came from a camera", not "no AI touched it afterwards".
- An extracted value **never overrides a declaration the maker made.** This is not a new rule —
  ADR 0081 §3's precedence (`extracted > default > empty`, never over a chosen value) already says
  it, and this decision instantiates that rule rather than adding one.

**4. Provenance ⊥ visibility. The flag is a filter and never a gate.** A viewer may hide AI work
from their own feed. That is a *filter*: the work stays public, findable and countable, and no
derived-copies obligation arises.

⛔ **It must not become a withholding.** The moment a provenance value hides work from other
people, every derived copy inherits the obligation — search text, facets, suggest, thumbhash, CLIP
embedding, counts, covers (the #1066 list) — and we would be standing up a **third** visibility
plane beside sensitivity and publication, for an axis that is a *statement about the work* rather
than a *permission on it*.

If an operator ever wants AI work off their instance, that is **moderation**: it produces an
ordinary withheld or removed state through the sensitivity/state machinery that already exists and
already carries the derived-copies discipline. The provenance column stays a statement. This is
the same separation ADR 0090 draws between rating and clearance, and for the same reason.

**5. One control, one interaction.** The upload surface presents a single three-position control
("No AI / AI-assisted / AI-generated"). That is the same one decision a checkbox asks for, so the
friction budget the owner set is met — three positions is not more friction than two, it is more
*resolution* on the same single act. It may be left untouched, which stores NULL.

**6. It does not federate yet, and the mapping is pre-decided so that it can.** The v1 envelope
parses strictly — unknown top-level fields are rejected, and new fields require an `@context`
version bump (`federation/envelope.go`). The existing per-work flag, `mature`, has not taken that
step either. So this ships local-only. When the context is next bumped, the wire form is
**already decided**: map `generated` → `trainedAlgorithmicMedia` and `assisted` →
`compositeWithTrainedAlgorithmicMedia`, and carry `none` as our own explicit term, because IPTC
has none and inventing an IPTC-looking value for it would be a lie in a standard vocabulary.

## Consequences

- The column is an enum (or a small lookup), nullable, on `assets`, with post derivation following
  the mature axis's shape — `recompute_post_*` plus a trigger on `post_assets`, a column and a
  trigger rather than a subquery. **Derivation takes the strongest member value**: a post
  containing one `generated` member is `generated`.
- Because it is a filter and not a gate, **no derived copy has to be withheld**, which is what
  keeps this cheap. That property is a consequence of decision 4 and disappears the moment
  decision 4 is revisited.
- Reading `Iptc4xmpExt:DigitalSourceType` is a **later, optional** enhancement: a namespace
  constant plus an `absorb` case in the XMP extractor, landing as a namespaced canonical field
  under ADR 0084 with `set_by = "xmp"`. It is not required for #1167 and must not be mistaken for
  the store.
- Still images are the only formats that could ever prefill. The declaration must stand alone for
  everything else, which is the majority of what this system holds.
- A future granular need (which model, which prompt — IPTC 2025.1 defines properties for both) is
  additive against an enum and would have been a second migration against a boolean.

## Alternatives considered

**Store `DigitalSourceType` directly as the field.** Rejected: cardinality 0..1 and no "no AI"
term, so the owner's explicit NoAI declaration is unrepresentable. It also imports a 17-term
production-method taxonomy as a *disclosure* control, which is a friction disaster at upload.

**A boolean, per the owner's literal "checkbox".** Rejected on the two grounds in decision 2 —
irrecoverable collapse of assisted/generated, and no room for undeclared. The owner's *intent*
(one control, minimal friction) is preserved by decision 5; only the storage type differs, and the
control they asked for is still a single interaction.

**Infer `none` when an extractor returns no AI term.** Rejected, and this is the trap the whole
pass exists to name: it manufactures a disclaimer the maker never made, on exactly the topic where
a false disclaimer is most damaging. Absence of a signal is not a signal.

**Let operators gate on it now.** Rejected as premature and expensive: it converts a statement into
a permission and drags every derived copy into the withholding discipline. Moderation already has
machinery for removing work.
