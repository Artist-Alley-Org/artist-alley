---
id: "0050"
title: Artist Alley as a Model Context Protocol (MCP) server
status: proposed
date: 2026-06-20
area: extensibility
phases:
  - "1.52"
supersedes: []
related:
  - "0034"
  - "0038"
  - "0042"
  - "0043"
tags:
  - extensibility
  - ai
  - integration
  - federation
excerpt: >-
  Expose the instance's asset catalogue via the Model Context Protocol so AI coding agents (Claude Code, Cursor, Codex CLI, etc.) and creative agents can query and reason over a studio's archive the same way they query a codebase.
---

## Context

The Model Context Protocol (MCP) is an emerging open standard for
exposing tools + data sources to AI agents. By the time this ADR is
implemented (post-1.14.A + 1.14.B), MCP will be the default
integration surface for Claude Code, Cursor, Codex CLI, Gemini CLI,
Aider, Zed, and several others. Studios increasingly run AI coding
agents alongside their human workflows; those agents currently see
the studio's *source code* (via tools like
[codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp))
but cannot see the studio's *art catalogue*.

artist-alley sits in the middle of this gap. The instance has the
typed asset model (Phase 1.9), the multi-provider AI inference
abstraction (Phase 1.14.A), the CLIP embeddings + similarity search
(Phase 1.14.B), the capability + audit + federation infrastructure.
Exposing this via MCP turns the instance into a first-class
knowledge source for any MCP-aware agent — a cinematics lead can
ask their AI agent "what shots have we approved that match this
storyboard mood?" and get answers grounded in the studio's actual
archive, not the agent's training data or a generic web search.

### Why the timing

Three preconditions need to be in place before this ships:

1. **AI provider abstraction (Phase 1.14.A)** so MCP tools that
   delegate to inference (summarisation, captioning of multi-asset
   results) have a stable callable surface.
2. **CLIP embeddings + pgvector (Phase 1.14.B)** so similarity-based
   MCP tools (`find_similar_to`, `find_by_visual_description`) have
   the data to query.
3. **Capability system (Phase 1.17)** so per-tool exposure can be
   gated cleanly without inventing a new auth model.

All three are either shipped (1.17) or have locked briefs (1.14.A).
This ADR documents the design ahead of implementation so the
provider abstraction + embeddings work doesn't accidentally close
off options that MCP would later need.

### Why MCP specifically (and not REST or GraphQL)

artist-alley already exposes a REST API + an ActivityPub-shaped
federation surface (per ADR 0043). MCP is additive — it's the
*agent-facing* protocol, not a replacement.

- **AI agents speak MCP natively.** Wrapping the REST API in an
  agent-side adapter is operator burden + brittle drift surface.
- **MCP is tool-first, not endpoint-first.** A tool definition
  carries its own JSON Schema for arguments + a description the
  agent can reason over. REST endpoints don't (OpenAPI gets close
  but agents have to consume + re-interpret).
- **MCP is the existing pattern for "expose data to agents".** The
  reference implementation surveyed (codebase-memory-mcp)
  validates that a single static binary serving MCP over stdio /
  HTTP is a tractable shape — operators install once, agents
  auto-discover.

The MCP surface delegates internally to the existing REST handlers
+ capability checks. No new auth model, no new permission system —
MCP is a thin protocol shim on top of the surface we already
maintain.

## Decision

artist-alley ships a Model Context Protocol server as a
first-class operator-toggleable surface. The implementation lives
in `app/internal/mcp/`; the protocol shim runs alongside the
existing HTTP API (same binary, separate transport).

### MCP tool surface (initial set; v1)

Each tool has its own capability gate. Operators expose only the
surface they're comfortable with. Defaults shipped:

| Tool | Capability | What it does |
|---|---|---|
| `asset.find_by_tag` | `mcp.assets.read` | Filter assets by one or more tags (AND / OR semantics) |
| `asset.find_similar` | `mcp.assets.similarity` | CLIP-embedding similarity search against a given asset or visual description (depends on Phase 1.14.B) |
| `asset.get` | `mcp.assets.read` | Get full asset metadata by ID (includes Phase 1.9 custom fields) |
| `collection.get` | `mcp.collections.read` | Get a collection's asset list + collection-level custom fields (per Phase 1.9.B) + relationships |
| `collection.search` | `mcp.collections.read` | Search collections by name / sensitivity / owning team |
| `post.find` | `mcp.posts.read` | Find posts matching tag / state / author filters |
| `summarise_assets` | `mcp.ai.use` | Multi-asset summarisation via Phase 1.14.A provider abstraction; respects privacy routing (Phase 1.14.A operator-locked constraint) |
| `propose_collection` | `mcp.ai.use` | AI proposes a curation based on a brief + a candidate asset pool |
| `audit.query` | `mcp.audit.read` (admin-only) | Query the audit log via MCP — surfaces "what AI calls did Claude make against my catalogue last week?" |

The full v1 tool surface caps at ~12 tools to keep the agent's
context manageable. Additional tools (write surfaces:
`asset.tag`, `asset.set_sensitivity`, `collection.create`) are
explicitly deferred — read-only first; write-side tools come in a
follow-up phase once the read surface has dogfood signal.

### Capability model

Six new capabilities seeded under the `mcp.*` namespace:

- `mcp.use` — umbrella; required to authenticate any MCP tool call
- `mcp.assets.read` — read tools that surface asset metadata
- `mcp.assets.similarity` — similarity search (depends on 1.14.B)
- `mcp.collections.read` — read tools that surface collection metadata
- `mcp.posts.read` — read tools that surface post metadata
- `mcp.ai.use` — tools that delegate to AI inference (cost + audit
  trail via Phase 1.14.A infrastructure)
- `mcp.audit.read` — admin-only audit query surface

Default seeded for the Base role: `mcp.use` + `mcp.assets.read` +
`mcp.collections.read` + `mcp.posts.read`. Similarity + AI delegation
+ audit query are operator opt-in (per the "minimize artist input"
principle's spirit applied to operator security — opt-in for
sensitive surfaces).

### Privacy posture

The MCP surface inherits the sensitivity-aware privacy posture from
Phase 1.14.A:

- Asset visibility through MCP respects existing share + team
  scoping. Agents see only what their authenticating user can see
  through the regular API.
- AI-backed tools (`summarise_assets`, `propose_collection`) route
  via the existing AI provider router → restricted / embargo assets
  forced to local providers (Ollama, vLLM) per the 1.14.A
  operator-locked default.
- Audit log records every MCP tool call (`mcp.tool.invoked` event)
  with the actor's API token reference + the tool name + the
  asset / collection IDs touched. Standard Phase 1.17.D changeset
  shape.

### Federation-awareness

artist-alley federates assets across instances (per ADR 0043). MCP
tools that surface federated content disambiguate via the existing
actor URI scheme:

- `asset.get` accepts both local UUIDs + federation actor URIs;
  remote assets resolved via the existing `remote_actors` cache
  (per Phase 1.22.I-c)
- Cross-instance similarity search is **out of scope for v1** —
  similarity is local-instance-only at first ship; cross-instance
  federation of embeddings would need its own ArchivePub extension
  (a future ADR if operator demand emerges)

### Transport

Two transports per the MCP spec:

- **stdio** — for operator-host installs (operator runs the MCP
  server alongside their agent on the same machine, authenticated
  via a local API token)
- **HTTP / SSE** — for remote-agent installs (operator's AI agent
  runs elsewhere, talks to the MCP server over HTTPS with token
  auth)

Both share the same handler implementation; the protocol shim
selects transport at startup.

### Configuration surface

A small admin UI under `/admin/mcp` exposes:

- Enable / disable MCP server entirely
- Per-tool exposure toggles (operator can hide individual tools
  even if the capability is seeded)
- Per-instance tool-call rate limits (independent of AI provider
  rate limits)
- Token-scoped tool whitelisting ("this token can call
  `asset.find_by_tag` but not `propose_collection`")

### Distribution

MCP server is **core**, not an add-on. It's a thin protocol shim;
the heavy work it delegates to lives in existing packages.
Operators who don't want it disable via the master switch.

### What this is NOT

- Not a replacement for the REST API. MCP is agent-facing; REST
  is everything-facing.
- Not a federation extension. Cross-instance MCP queries are
  out of scope for v1.
- Not a write-side surface in v1. Read-only at first ship.
- Not a hosted SaaS offering. Self-hostable like the rest of
  artist-alley. (A future cloud-bridge phase per ADR 0038 might
  offer hosted MCP for studios who want it, but that's separate.)
- Not exposing AI-agent code-context. We don't ship a
  codebase-memory-mcp clone; we ship an asset-catalogue MCP.
  The two are complementary, not competing.

## Consequences

**Positive:**

- Studios running AI coding agents alongside their pipeline get a
  unified knowledge surface — the codebase via codebase-memory-mcp
  (or similar), the asset catalogue via artist-alley MCP.
- Operator-grade observability: every agent query audits + every AI
  delegation cost-tracks via Phase 1.14.A infrastructure.
- Federation gets a new value vector: federated instances become
  cross-studio knowledge sources for agents (with the obvious
  privacy + sensitivity caveats from 1.14.A).
- The MCP standard is portable across major agent vendors; we don't
  bet on a single ecosystem.

**Negative:**

- New protocol surface to maintain. MCP is still evolving; spec
  churn possible until v1.0.
- Read-only first means the early surface is limited; some
  operator expectations may need re-management ("can my agent
  approve these assets?" — not yet).
- The audit-log volume increases substantially if agents are
  noisy. Phase 1.40 retention policies become more important.

## Alternatives considered

- **Just expose REST + let agents adapt.** Considered; rejected
  because agent-side adapters are brittle + create operator
  burden. MCP is the right abstraction layer.
- **Wait until MCP v1.0 stabilises before shipping.** Considered;
  rejected because the protocol is already widely-deployed in
  agent vendors. The cost of being slightly ahead of spec
  finalisation is small compared to the cost of being late.
- **Build a custom artist-alley-only agent protocol.** Rejected
  immediately — would re-invent every interop problem MCP solves.

## Reference

- Model Context Protocol specification: https://modelcontextprotocol.io/
- codebase-memory-mcp (reference implementation surveyed): https://github.com/DeusData/codebase-memory-mcp
- ADR 0042 — Distributed catalogues, typed constants per package
- ADR 0034 — Capability add-ons
- ADR 0043 — Federation walled-garden protocol
- Phase 1.14.A focused brief (drafted 2026-06-20) — AI provider
  abstraction; the inference surface MCP delegates to
- Phase 1.14.B (planned) — CLIP embeddings + similarity search
- Phase 1.17 — Identity & teams (the capability + audit
  infrastructure this design leans on)
