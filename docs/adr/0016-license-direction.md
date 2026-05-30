---
id: "0016"
title: License direction — toward AGPL + commercial dual-license
status: accepted
date: 2026-05-29
area: licensing
phases: 
  - "1.23"
supersedes: 
  - "0002"
related: 
  - "0002"
  - "0011"
  - "0017"
  - "0018"
tags:
  - licensing
  - ai
  - 3d
excerpt: >-
  ADR 0002 placed the project under BSD-3-Clause on the explicit premise that "we have no monetization intent for artist-alley itself." That premise no longer holds.
---
## Context

ADR 0002 placed the project under BSD-3-Clause on the explicit premise that
"we have no monetization intent for artist-alley itself." That premise no
longer holds.

The project has matured into a viable product for game studios. We now
want to sustain its development through commercial revenue — support
contracts, hosted offerings, and an Enterprise feature tier (see
ADR 0017). BSD-3-Clause is structurally hostile to that goal: it places
no constraint on a hyperscaler standing up a managed artist-alley service,
charging for it, and contributing nothing back. The exact failure mode
that drove Sentry, Elastic, MongoDB, HashiCorp, Mattermost, and Redis off
permissive licenses applies to us word-for-word.

Two facts make a relicense newly tractable:

1. **The Strangler Fig pattern was abandoned in favour of clean-room
   greenfield Go** (see memory `project_strangler_fig_abandoned`). Almost
   all production code in `app/`, `web/`, and `site/` is original work,
   not RS-derived. The ResourceSpace codebase is now used only as a
   *blueprint* — a reference implementation we audit when designing
   feature parity, not a substrate we extend.
2. **The ResourceSpace fork lives at the repo root in `api/`, `batch/`,
   `include/`, `lib/`, `pages/`, etc.** It is being removed, not
   extended. Once it is gone, no BSD-3 obligation constrains the
   replacement code.

## Decision

The project will move to a **dual-license model**:

- **AGPL-3.0** for the open-source community distribution.
- A separate, paid **commercial license** for customers who need to embed
  artist-alley in a proprietary product, run it as a paid hosted service
  without contributing back, or otherwise sidestep AGPL's
  network-copyleft obligations.

The relicense will happen in two phases:

### Phase 1 — Audit and excise RS-derived code (pre-relicense)

Before any relicense, complete an audit that establishes:

- Which files in the working tree are RS-derived (still under BSD-3 by
  the original license contract).
- Which files are clean-room original work we can relicense unilaterally.

Files in the second bucket are safe to relicense. Files in the first
bucket must either be removed (preferred, since the strangler approach is
already abandoned) or kept under BSD-3 with a clear `// rs-derived`
marker if we choose to retain them.

Practically, this means the audit and the deletion of the residual RS
fork in `api/`, `batch/`, `include/`, `lib/`, `pages/`, etc. happen
together. Once those directories are gone, the audit is the simple "all
remaining tracked files are clean-room" check.

### Phase 2 — Relicense

When the audit confirms a clean tree:

- Update the root `LICENSE` to AGPL-3.0.
- Add a `LICENSE.commercial.md` describing the dual-license offer.
- Add a `CONTRIBUTING.md` clause requiring a **Contributor License
  Agreement (CLA)** before any external contributions land. The CLA
  assigns to us (or grants us joint rights to) the contributor's work,
  which is what makes dual-licensing legally possible — we cannot offer a
  proprietary license for code we don't have the right to relicense.
- The ResourceSpace `license.txt` and `documentation/licenses/` directory
  remain for historical attribution of the *removed* upstream code.

## Constraints and compatibilities

- **Blender bundling stays unaffected.** Blender is GPL-2.0+; AGPL is
  compatible with GPL when used in "mere aggregation" — i.e. Blender
  runs as a subprocess and we do not link into its libraries via Cgo
  (see ADR 0018, planned). The standard subprocess pattern for `.blend
  → glTF` conversion remains fully legal.
- **Plugin ecosystem (ADR 0011, future Phase 1.23).** AGPL's
  network-copyleft does not extend to plugins that interact via a defined
  RPC boundary (WASM via Extism), so plugin authors retain license
  freedom for their plugin code. This is the same shape Gitea, GitLab,
  and Mattermost use.
- **Studio self-host stays trivial.** AGPL imposes no obligations on a
  studio that runs artist-alley on its own infrastructure for its own
  employees — the network-copyleft trigger requires offering it as a
  service to third parties. Game studios self-hosting for internal use
  remain free to modify and never publish back.

## Consequences

**Positive**

- A hyperscaler cannot stand up a paid managed artist-alley service
  without either publishing their changes (including any de-license
  patches) or buying our commercial license. This is the actual moat.
- Commercial customers have a clear path: pay for the commercial license,
  get proprietary embedding rights + Enterprise support contract.
- Community customers keep full source access, modification rights, and
  the right to self-host without any commercial obligation.
- The dual-license model is well-understood by procurement at any AAA
  studio — there is no novel legal concept for buyers to evaluate.

**Negative**

- A CLA is friction for external contributors. We must accept this.
  Mitigation: clear, short CLA modelled on the Apache CLA.
- An RS-derived file audit is real work. Mitigation: pair it with the
  already-planned removal of the residual RS fork.
- Until the audit completes and relicense lands, we remain BSD-3 and
  technically vulnerable to the fork-as-SaaS risk. This is acceptable
  given pre-MVP status — there is nothing yet worth forking.

## Alternatives considered

- **Stay BSD-3-Clause.** Rejected: incompatible with sustainable
  monetization, see context.
- **Move to BUSL (Business Source License).** Time-bounded
  non-compete, converts to OSS after N years. Rejected: BUSL is *not*
  OSI-approved, which trips compliance review at some procurement
  pipelines, and it muddies the "OSS community edition" story we want
  Community users to feel.
- **AGPL only, no commercial.** Rejected: leaves no path for customers
  who need proprietary embedding rights. Forces those customers to a
  competitor instead of paying us.
- **Open core (closed-source paid features in a private repo).**
  Rejected per ADR 0017 — breaks studio source-audit requirements for
  AAA procurement.

## Reference

- ADR 0002 — original BSD-3 decision (superseded by this ADR).
- ADR 0017 — monetization model & technical license enforcement (consumes
  this ADR's license direction).
- ADR 0018 (planned) — Blender packaging as optional worker container.
