---
id: "0003"
title: Strangler Fig pattern applied internally
status: superseded
date: 2026-05-23
area: architecture
phases: []
supersedes: []
superseded_by: "0006"
related: 
  - "0001"
  - "0005"
tags:
  - architecture
  - ai
  - 3d
excerpt: >-
  ADR 0001 establishes that we hard-fork from RS and do not track upstream. That gives us total control of the codebase but does not by itself answer the question: how do we evolve from "RS with a coat of paint" toward the artist-alley vision (modern frontend, AI-native search,…
---
## Context

ADR 0001 establishes that we hard-fork from RS and do not track upstream.
That gives us total control of the codebase but does not by itself answer
the question: **how do we evolve from "RS with a coat of paint" toward the
artist-alley vision (modern frontend, AI-native search, real-time review,
3D viewer) without rewriting everything at once?**

A solo developer cannot replace 9,651 PHP files in a useful timeframe.
We need a pattern that lets us ship value continuously while gradually
shifting the application's center of gravity off of legacy PHP and onto
the architecture we want.

## Decision

Apply the **Strangler Fig** pattern internally to our fork. Concretely:

1. **RS PHP runs the application on day one.** Nothing is broken; the
   application functions as a complete DAM from the moment Phase 0 ships.

2. **New capabilities are added as sidecar services**, not as new PHP
   modules. The `services/` directory holds Go (default) and Python
   (where ML stacks demand it) microservices that own new functionality:
     - `services/ai-gateway` — provider-agnostic AI abstraction
       (Anthropic, OpenAI, Azure OpenAI, Ollama, vLLM, OpenAI-compatible)
     - `services/review-sessions` — SSE-based live review sessions
     - `services/video-pipeline` — HLS + frame-scrub transcoding
     - `services/embeddings` — vector generation, swappable backend

3. **The frontend is incrementally replaced** by a SvelteKit application
   in `web/`. RS PHP pages remain functional and continue to serve admin
   and long-tail features. The SvelteKit app gradually takes over the
   artist-facing pages (upload, browse, asset detail, review) as each is
   reimplemented.

4. ~~A second database (PostgreSQL + pgvector) is added alongside RS's
   MySQL, not as a replacement. New-feature data (review sessions,
   embeddings, modern annotations, comments v2) lives in Postgres, linked
   to RS resources by `resource_id`. No cross-DB joins; composition lives
   at the application layer.~~ **(Superseded by ADR 0005 — single Postgres
   for both RS and artist-alley additions; MySQL dropped entirely.)**

5. **RS PHP is modified in place** when a sidecar needs to integrate
   (e.g. hooks that publish events when resources are uploaded). We
   tag every modification with an `// ARTIST-ALLEY:` marker so we can
   grep for our diffs against the original fork point.

6. **Eventual replacement of RS PHP modules** happens only when a sidecar
   has reached parity *and* the PHP module is a maintenance burden. We
   are not in a rush. Many RS modules (permissions, resource types,
   metadata field schemas, audit logs) may live forever — they work,
   they're battle-tested, and we have no reason to throw them out.

## Consequences

**Positive**
- Every phase of work ends with a shippable, complete application.
- Solo-dev pace is sustainable: each sidecar is a discrete project.
- Risk is bounded: a failed sidecar experiment doesn't break the DAM
  underneath.
- Modern stack choices for new code (Go, SvelteKit, Postgres, SSE) are
  not held back by legacy PHP constraints.

**Negative**
- Two DBs to operate (MySQL + Postgres) until/unless we consolidate.
- Two languages and two web frontends coexist for a long time. We must
  document the seam between RS PHP pages and SvelteKit pages clearly so
  contributors aren't confused about which serves what.
- Some integration boilerplate (PHP-to-Go RPC, event hooks) is needed
  that wouldn't exist in a monolith.

## How to apply this in day-to-day work

When adding a new capability, ask in order:

1. Does an RS plugin or core module already do this acceptably? **If yes,
   use it.** Do not duplicate.
2. Can this live entirely in a sidecar service with no PHP changes?
   **If yes, build it there.**
3. Do we need PHP to publish an event or expose data to the sidecar?
   **Modify PHP minimally, tag with `// ARTIST-ALLEY:`, keep the sidecar
   as the brain.**
4. Is an RS module actively harmful to the vision and is the sidecar at
   parity? **Only then consider replacing the PHP module.**
