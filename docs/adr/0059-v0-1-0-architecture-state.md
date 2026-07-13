---
id: "0059"
title: v0.1.0 architecture state — the release bundle as a decision set
status: accepted
date: 2026-07-13
area: architecture
phases:
  - "1.55"
supersedes: []
related:
  - "0016"
  - "0035"
  - "0040"
  - "0046"
  - "0047"
  - "0057"
tags:
  - release
  - licensing
  - packaging
  - migrations
  - governance
excerpt: >-
  v0.1.0 (2026-07-11) is the first public tag, and it shipped as a
  bundle of coupled decisions rather than a feature release: org move
  to Artist-Alley-Org, site split, AGPL-3.0-only + dual commercial
  relicense, migration squash point #1, Docker-only distribution, and
  full removal of RS-derived code. This ADR records that bundle in one
  place so later readers don't have to reconstruct it from seven
  earlier ADRs, and records v0.1.1 (2026-07-13) as the first
  append-only patch proving the release train works.
---

## Context

The v0.1.0 tag (2026-07-11) was deliberately more than a version
number. A set of long-gated decisions — licensing, repository
governance, distribution shape, schema baseline — were all scheduled
to execute "at the first public tag" because each one is cheapest at
the moment the repo goes public and history is rewritten. They landed
together in the Phase 1.55 release-readiness arc.

Each decision has its own ADR, but the *bundle* — what actually
became true at the tag, all at once — is recorded nowhere. This ADR
is that record: a snapshot of the architecture and governance state
as of v0.1.0, plus the v0.1.1 patch that immediately followed.

## Decision

The following are the facts of record as of the v0.1.0 tag:

1. **Canonical org is Artist-Alley-Org.** The repository moved from
   `mscrnt/artist-alley` to `Artist-Alley-Org/artist-alley` at the
   tag. Repo URLs are canonical under the org from here on. The Go
   module path `github.com/mscrnt/artist-alley/app` is **deliberately
   retained** as a stable identifier — GitHub redirects make it
   resolve, and churning every import path buys nothing. Satellite
   repos (forked deps, side tools) have not moved; their homes are
   still undecided.

2. **Site split.** The `artist-alley.org` docs site is a private
   repo (`mscrnt/artist-alley-site`) that pulls the OSS docs from
   `dev` at build time via the self-hosted runner (Phase 1.55.Z).
   The ADR source of truth (`docs/adr/`) stays in this repo; the
   render pipeline (`sync-adrs.mjs`, the Astro components) lives in
   the site repo. This narrows [ADR
   0035](/adr/0035-adr-conventions/)'s pipeline-location claims:
   authoring conventions in 0035 stand unchanged, but every `site/`
   path it references now resolves in the site repo.

3. **Distribution is Docker-only until v1.0.0.** Images publish to
   GHCR (`ghcr.io/artist-alley-org/artist-alley`) and Docker Hub,
   multi-arch (amd64 + arm64), Sigstore-signed with SBOM +
   provenance. This re-dates [ADR
   0047](/adr/0047-cross-platform-packaging/) without superseding
   it: the three-platform end-state (native packages, static
   binaries, Homebrew) remains the target, moved to v1.0.0.

4. **License is AGPL-3.0-only + dual commercial**, executed
   2026-07-11 in Phase 1.55.AA per [ADR
   0016](/adr/0016-license-direction/). The first public commit is
   AGPL from the start; every non-generated source file carries an
   SPDX header; the dual model is documented in `LICENSING.md`.

5. **Migration squash point #1 executed** per [ADR
   0046](/adr/0046-migration-baseline-and-squash-policy/) and [ADR
   0057](/adr/0057-v0-1-baseline-schema-shape/). v0.1.0 ships exactly
   one migration, `app/internal/db/migrations/00001_baseline_v0_1.sql`.
   Everything after the tag is append-only within the 0.x line until
   squash point #2 at v1.0.0.

6. **RS-derived code is fully removed.** The 1.55.S sanitization pass
   confirmed no ResourceSpace-derived code ships in the tag, and the
   gitignored RS reference tree was physically deleted. ADRs 0001,
   0003, and 0015 (the fork-and-port lineage) are permanently
   historical; the clean-room methodology of [ADR
   0040](/adr/0040-clean-room-reverse-engineering-methodology/)
   governs any future blueprint reading.

7. **v0.1.1 (2026-07-13) is the first append-only patch** and the
   proof the release train works end-to-end: worker-pool claim fix
   (#279) restoring media processing, GHCR owner-casing fix (#280),
   and dependency cleanup (#281 / #283 / #284) clearing all open
   Dependabot alerts. Tag → build → sign → publish ran twice in
   three days without manual surgery.

## Consequences

- Later ADRs and docs can cite this one instead of re-deriving "what
  was true at v0.1.0" from seven places.
- Anything still carrying pre-tag claims (mscrnt-hosted repo links,
  pre-fold migration filenames, three-platform packaging as current
  fact) is stale by definition and should be corrected against this
  record when touched.
- The retained `mscrnt` Go module path is a conscious trade: import
  stability over branding purity. Revisit only if GitHub redirect
  semantics ever change.
- The append-only window is now open: schema changes land as new
  numbered migrations on top of the baseline until v1.0.0.

## References

- [ADR 0016](/adr/0016-license-direction/) — license direction,
  executed at this tag.
- [ADR 0035](/adr/0035-adr-conventions/) — ADR conventions; pipeline
  location narrowed by the site split.
- [ADR 0040](/adr/0040-clean-room-reverse-engineering-methodology/) —
  clean-room methodology, now the sole governing process for
  blueprint reading.
- [ADR 0046](/adr/0046-migration-baseline-and-squash-policy/) — squash
  policy; point #1 executed here.
- [ADR 0047](/adr/0047-cross-platform-packaging/) — packaging
  end-state, re-dated to v1.0.0.
- [ADR 0057](/adr/0057-v0-1-baseline-schema-shape/) — the baseline
  schema this tag ships.
