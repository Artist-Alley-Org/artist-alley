---
id: "0084"
title: Every supporting extractor runs, and canonical fields are namespaced per source
status: accepted
date: 2026-08-02
area: architecture
phases: []
supersedes: []
related:
  - "0012"
  - "0081"
tags:
  - metadata
  - extraction
  - provenance
excerpt: >-
  Until #828 the extract dispatcher stopped at the first extractor whose
  Supports() said yes — EXIF registers first and claims image/jpeg, so the
  IPTC and XMP extractors never ran on any JPEG in production. This records
  the replacement: fan out to every supporting extractor, merge with
  per-source namespaced canonical fields, and reconcile semantics at the
  operator's wiring choice rather than in the merge.
---

## Context

The metadata extract job picked its extractor like this:

```go
for _, e := range h.extractors {
        if e.Supports(mimeType) { ext = e; break }
}
```

EXIF, IPTC and XMP all answer yes for `image/jpeg`, and EXIF is registered
first. So on every JPEG ever uploaded, **only the EXIF extractor ran**. The
IPTC and XMP extractors — their parsers, their carrier walkers, their
twenty-odd `CanonicalField`s — could not be reached from production at all.
They were exercised solely by their own unit tests, which is why nothing said
so. Four of the seed dataset's images carry a rights statement in XMP and
nothing in EXIF: the files that most needed reading were exactly the files
nothing read.

This surfaced while wiring the shipped field catalogue (#800): a field wired
to `iptc_credit` or `xmp_rights` would have routed nothing, silently — no
failure row, no log line. The wiring work was structurally impossible without
changing the dispatch.

The three namespaces are complements, not alternatives. EXIF describes the
camera, IPTC the newsroom, XMP the rights; a photograph routinely carries all
three, saying different things.

## Decision

**1. Fan out.** Every extractor whose `Supports()` answers yes for the MIME
type runs. The source is buffered once (each extractor `ReadAll`s its reader
anyway, so peak memory matches the old single-extractor path and storage is
read once instead of N times). One extractor failing hard does not stop the
others; an unknown error class aborts for the job framework's retry. "No
extractor succeeded" is distinguished from "the file has no metadata" so
counter classification is preserved.

**2. Canonical fields are namespaced per source, and that is the design, not
an accident.** `capture_datetime` is EXIF's, `iptc_credit` is IPTC's,
`xmp_rights` is XMP's. Two extractors' views of the *same semantic* (an EXIF
Copyright and an XMP `dc:rights`) are different canonical fields, and they are
reconciled **by the operator, one level up**, by choosing which canonical a
field definition's `extraction_source` names. The merge never decides which
namespace's truth wins a semantic argument.

**3. Merge precedence is a determinism guarantee, not a policy.** Results
fold first-writer-wins in extractor registration order (EXIF, IPTC, XMP). In
practice nothing collides, because of §2. If a collision ever fires, the
right fix is a distinct `CanonicalField`, not a cleverer merge.

**4. Provenance is carried per field.** After a merge the result is no longer
one extractor's word, so `set_by` records the extractor that actually
produced each value (`exif`/`iptc`/`xmp`/`pdf`/`raw`), with `extract` as the
honest fallback for results assembled without provenance. This is what makes
ADR 0012's provenance model and ADR 0081 §3's precedence
(`extracted > default > empty`, never over a chosen value) auditable.

## Consequences

- Wiring a field to an IPTC or XMP canonical now routes. Before this, such a
  wire was a phantom control — configured, displayed in the admin, and dead.
- Every JPEG upload runs up to three parsers instead of one. They are
  in-memory parsers over an already-buffered source; storage is still read
  once. No I/O is multiplied.
- A new extractor added to the registry runs alongside the others by default.
  The registration-order comment ("EXIF first — largest catalog") now
  describes merge determinism rather than a dispatch winner.
- The unreal-fixture lesson (ADR 0068) gains its largest instance yet: two
  entire extractors were green under test and unreachable from production.
  A subsystem's tests passing says nothing about whether the subsystem is
  ever invoked — reachability needs its own assertion, which the wiring
  tests now provide end to end.
