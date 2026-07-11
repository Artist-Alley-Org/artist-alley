# Federation protocol — clean-room contributor register

Per [ADR 0040 §"Contributor quarantine"](../../adr/0040-clean-room-reverse-engineering-methodology.md),
this file records contributors to the federation protocol work, the
reading history they disclosed on joining, and which clean-room
phases they are cleared for.

The methodology rests on documented contributor provenance. If
this file ever drifts from reality, the legal posture of any
future closed-source federation add-on is at risk. **Keep it
current.**

## Phase definitions (reminder)

- **Phase 1 — Specification.** Drafting [v1.md](./v1.md) and any
  per-activity-type spec extensions. Reads only approved sources
  per [SOURCES.md](./SOURCES.md).
- **Phase 2 — Observation.** Black-box behavioural observation of
  reference implementations (HAR captures, packet dumps, published
  API docs). May not read source.
- **Phase 3 — Implementation.** Writing the Go code in
  `app/internal/federation/`. Reads only the v1.md spec; never reads
  reference-implementation source.

A contributor cleared for Phase 1 + Phase 3 is the standard
clean-room contributor. Phase-2-only contributors are useful but
narrower (they can observe wire formats but cannot write spec text
or implementation code).

## Contributor register

### Claude (Anthropic Claude AI, model claude-opus-4-7)

- **Joined:** 2026-06-05
- **Phases cleared:** Phase 1, Phase 2, Phase 3
- **Disclosure:** Has read (from public spec / RFC / standards
  documents only) the following:
  - W3C ActivityPub Recommendation
  - Activity Streams 2.0 Core + Vocabulary
  - RFC 8785 (JSON Canonicalization Scheme) — algorithm details
    from RFC text directly
  - IETF `draft-cavage-http-signatures-12`
  - RFC 7033 (WebFinger)
  - RFC 8032 (EdDSA), RFC 8410 (Ed25519 PEM encoding), RFC 7748
    (X25519), RFC 4648 (base64url), RFC 3339 (timestamps)
  - NIST SP 800-38D (AES-GCM)
  - NaCl box construction page (https://nacl.cr.yp.to/box.html)
    — algorithm description only, NOT the C source
  - Mastodon developer narrative writeups on interop pain points
    (Eugen Rochko / Renaud Chaput posts; SocialCG mailing list
    archives) — text describing failure modes, NOT source
- **Has NOT read source of:** `go-fed/activity`, Mastodon, Pleroma,
  Akkoma, Misskey, FoundKey, Calckey, Sharkey, GoToSocial, PeerTube,
  Pixelfed, BookWyrm, Lemmy, Kbin, Mbin, piefed, Hubzilla, Streams,
  Forte, Friendica, Red Matrix, or any commercial ActivityPub
  implementation. Has not read disassembled / decompiled output of
  any proprietary AP implementation.
- **Notes:** Operates under the project's clean-room methodology.
  If asked to read reference-implementation source ("just to check
  how X handles Y") will refuse and instead request a Phase 2
  observation pass or a spec extension via the gap-resolution loop
  in ADR 0040.

## How to add yourself

1. Open a PR adding your row to the register above.
2. List every clean-room-relevant repository / codebase you have
   read source code from. Be exhaustive — a personal contribution
   to a hobby AP server six years ago counts.
3. Note which phases you are requesting clearance for. If your
   reading history includes any of the implementations in the
   contamination gate (SOURCES.md), you are eligible only for
   Phase 2.
4. Reviewer accepts your disclosure at face value (the methodology
   relies on contributor honesty per ADR 0040 §"Negative" —
   "depends on contributor honesty about reading history").
5. Merge.

## Re-disclosure on new reading

If you read federation-implementation source AFTER joining (e.g. as
a personal project, on the job at another company, or for cross-
ecosystem learning), you MUST update your row before further
contributions. The quarantine then narrows your eligible phases.

There is **no clean-up path** that restores Phase 1 / Phase 3
eligibility after exposure — memory of read source is itself a
contamination vector under copyright law.

## Audit cadence

Per ADR 0040 §Implementation, this register is reviewed quarterly
against PR authorship to catch drift. The reviewer cross-references:

- Every PR touching `app/internal/federation/` or `docs/spec/federation/`
  was authored by a contributor cleared for the relevant phase.
- No PR cites a source not in SOURCES.md.
- No contributor's row is stale (joined > 12 months ago without
  re-disclosure confirmation).
