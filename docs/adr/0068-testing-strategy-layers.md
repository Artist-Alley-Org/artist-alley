---
id: "0068"
title: Testing strategy — catch the class, not the instance
status: accepted
date: 2026-07-21
area: process
supersedes: []
related:
  - "0058"
  - "0067"
  - "0080"
tags:
  - testing
  - quality
  - ci
excerpt: >-
  #475 shipped green — no test caught a dead internal link because the suite
  walked posts (which work) but never collection→asset→viewer, and the
  not-found spec only asserted that bogus routes 404, never that real routes
  don't. The standard: layered coverage that catches a whole class, red-first
  regression with every fix, robustness over speed.
---

## Context

**#475 shipped with green CI.** vitest, svelte-check, the "Playwright standalone" job (~30
specs), and the full-stack integration suite all passed while every asset click inside a
collection 404'd. The suite had the pieces and still missed it:

- `ui-15-post-detail-modal` clicks through to the viewer — but only for **posts** (the path that
  works).
- `ui-18-collection-fields` opens a collection — but only to test the *fields editor*; it never
  clicks an asset tile.
- `ui-29-not-found` asserts a **bogus** URL renders a friendly 404 — it never asserts that a
  **real** navigation target does **not** 404. That is the exact inverse of what #475 needed.

The pattern is that the expensive bugs live in the **seams** between working parts — a dead
internal link, a row-plane/content-plane disagreement (#460/#476), a viewer that doesn't handle
gated content (#471). Unit tests over individual components pass; the integration of "real user
clicks real link, real surface renders" is untested. The operator's direction was explicit:
**"I want tests that are robust. I don't need quick, I need right. I don't want to run into an
issue that could have been caught with a test."**

## Decision

A **layered** testing standard. Each layer catches a *class* of defect the layers above cannot,
and the cheap layers do not excuse skipping the expensive ones — robustness over speed.

1. **Regression-with-fix (non-negotiable).** Every bug fix ships a test in the **same PR** that
   is **red without the fix** and green with it. A fix without a failing-first test is
   incomplete. #475's fix (ADR 0067) ships with layers 2 and 3 below.

2. **Static route/link integrity (CI, no stack).** A check that enumerates every internal
   `href="/…"` literal in `web/src` and resolves it against the SvelteKit route table
   (`src/routes/**/+page`, honoring `[param]` and route groups), failing on any target with no
   matching route. Catches the entire dead-link class — including #475 — in seconds, with no
   running stack. This is the net #475 most obviously lacked.

3. **E2E golden-path coverage (seeded stack).** Playwright specs that walk the *real navigation
   flows* end to end and assert both that the surface renders **and** that no `4xx` to `/api/`
   and no error boundary fired: browse→collection→asset→viewer, post→viewer, featured→target,
   search→result. Known-benign console noise (e.g. the analytics beacon's CORS line) is
   allowlisted so the assertion fails only on real defects. The seeded stack — the same dataset
   the demo runs — is the fixture. A route with **zero** golden-path coverage is a reported gap,
   not a silent one.

4. **Plane-consistency integration (Go).** For each entity, tests asserting the row plane and
   content plane **agree**: a caller denied the row is denied the content, and the serving
   endpoints (variants/file/iiif) enforce the same predicate the browse path does — across the
   anon / authenticated / limited-viewer matrix. This is where #460/#476 live.

**Principle: prefer catching the class over the instance.** A per-instance regression test
(layer 1) proves *this* bug is dead; the class-level nets (layers 2–4) prevent its siblings.
Both are required — the instance test documents intent, the class net prevents recurrence.

## Consequences

- **v0.5.1** ships #475's fix with layers 1–2 (regression E2E + the link-integrity check),
  because "the test accompanies the fix" and the link-integrity check is the class-level net for
  exactly this bug.
- **v0.6.0+** builds out layer 3 (golden-path E2E across the seam flows) and layer 4
  (plane-consistency + role matrix), as a coverage epic — informed by, not blocking, the patch.
- Open bugs #471 / #476 / #470 each carry their own red-first regression test into their fix PR.
- A green CI that skipped a layer is not "covered" — the layer's absence is a tracked gap, logged,
  never silent.

## Amendment 2026-07-29 — a fixture must be able to exercise the thing it claims to cover

Three bugs in one day shipped green because a test was written to agree with an
**assumption** rather than with the format, the API, or production reality. This is
not the same failure as a missing layer, and no layer above catches it: the test
exists, runs, and passes.

- **#750.** `TestResolveCompanions_SelfContained` wrote the eleven-byte ASCII string
  `"glTF binary"` to `model.glb` and asserted "GLB should declare no companions." It
  passed because there was nothing to parse. The assertion was true of the fixture and
  false of every real GLB — 363 of 374 in the catalogue reference external textures.
  The test did not merely miss the bug; it **certified the false assumption** and would
  have failed anyone who fixed it.
- **#689.** The 3D smoke test asserted the rendered poster had non-transparent pixels.
  Untextured flat-white geometry is opaque, so an entirely untextured catalogue passed
  twice.
- **IIIF / EXIF dimensions** (earlier): a green suite against a state production could
  not reach, because `assets.has_image` had no writer.

**The rule:** a fixture must be capable of reaching the failure the test claims to
guard. Before writing an assertion, ask **what would make this fail?** — and confirm
that a real input *can* produce that outcome. Two practical forms:

1. **Assert on the discriminator, not a side effect.** "Non-transparent pixels" is
   satisfied by the bug; "the material carries a decoded texture" is not.
2. **Build negative fixtures from the real producer.** #752's replacement tests the
   self-contained case with bytes `WriteGLB` itself emits, so "no companions" is a fact
   about a genuine GLB rather than about a text file with a misleading name.

**Corollary for comments.** Each of these bugs also sat behind a comment asserting the
property as settled — `worker.mjs` claimed it shared the viewer's loaders; the companion
resolver claimed "GLB/FBX are self-contained." **A comment asserting a structural
guarantee is worse than no comment, because it stops the next person looking.** If a
comment states a guarantee, either a test enforces it or the comment says it is an
assumption. ADR 0069 carries the same lesson for the renderer specifically.

## References

- ADR 0058 — demo seed dataset (the golden-path E2E fixture).
- ADR 0067 — the /assets/[id] route whose fix pilots this standard.
- #475 — the bug that shipped green and motivated this ADR.
