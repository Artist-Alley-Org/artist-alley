---
id: "0051"
title: Artist Alley as a Model Context Protocol (MCP) client
status: accepted
date: 2026-06-20
area: extensibility
phases:
  - "1.53"
  - "1.14.E"
supersedes: []
related:
  - "0034"
  - "0042"
  - "0050"
tags:
  - extensibility
  - ai
  - integration
  - mcp
excerpt: >-
  Consume external MCP servers as tool sources from AA's AI orchestrator. Inverse of ADR 0050 — instead of AA exposing its catalogue to external agents, AA's own provider abstraction calls out to operator-registered MCP servers (ComfyUI, custom studio bridges, etc.).
---

## Status (2026-06-22)

Foundation shipped via PR #154 (Phase 1.53.A): `mcp_server_registration` + `mcp_server_tool_grant` tables (migration 00013), `mcp.client.{use,admin,images.read,images.write}` capabilities, `mcpdispatch.Dispatcher.Invoke` 6-step guard chain (server-resolve → caller cap → tool whitelist + per-tool cap → privacy class vs sensitivity → budget gate → provider call + audit), per-server health-check goroutine, JSON-RPC over HTTP provider (`mcp_server.Provider`), admin UI at `/admin/ai/mcp-clients`.

First internal caller shipped via PR #156 (Phase 1.14.E-1): `app/internal/aiedit/` subsystem with `ImageEditProvider` interface, ComfyUI-via-MCP provider, `creative_lineage` table (migration 00014), async img2img job, `POST /assets/{id}/edit/img2img` endpoint, viewer `Img2ImgPopover` trigger. Operator-side ComfyUI MCP bridge ships at `tools/comfyui-mcp-bridge/` (Python 3.11+; HTTP + stdio transports; 5 typed tools + workflow drop-in discovery). Flux Kontext Dev validated end-to-end on operator's ComfyUI (14s wall-clock on RTX 5090 at 1024-edge).

Remaining (1.14.E-2 / 1.14.E-3): the other four image-edit operations (inpaint / outpaint / variations / remove-bg) with mask drawing UI; tier-aware provider routing gated on the licensing arc.

## Context

ADR 0050 commits AA to exposing its asset catalogue *as* an MCP
server (Phase 1.52) — agents like Claude Code or Cursor query AA.
That covers half the integration story. The other half: AA's own
AI orchestrator wants to consume external services as tools, and
the MCP ecosystem is increasingly where those tools live.

The pattern is validated by the user's
[SceneWeaver](https://github.com/mscrnt/SceneWeaver) reference
implementation. SceneWeaver wraps each external service (Tessa,
ComfyUI, Tractor, Shotgun, etc.) as a dedicated MCP server, then
its FastAPI backend calls those servers as tools through a single
`MCPClient`. The "wrapper-per-service + central orchestrator"
pattern composes cleanly and keeps each external integration's
auth + transport concerns isolated.

The most concrete near-term consumer for AA is **ComfyUI** for
operator-side local image generation. The existing ADR 0026 design
for AI creative editing (Phase 1.34) already names
`comfyui-local` as a provider. That provider can ship today as a
bespoke HTTP client OR as an MCP client — and MCP is the cleaner
abstraction because:

1. Other operators will want to plug in other local services
   (custom render farms, custom asset-validation tools,
   internal-only AI services). MCP gives them a registration
   point without per-service code in AA.
2. The MCP wire is stable + spec'd; bespoke HTTP integrations
   drift.
3. The audit + cost-tracking + privacy-routing infrastructure
   from Phase 1.14.A applies uniformly — every MCP tool call
   records like every other AI provider call.
4. Operator-registered MCP servers can be capability-gated by
   tool, not just by server.

### What this is NOT

- Not a Tessa integration. The user has explicitly stated AA
  will never integrate with Tessa. SceneWeaver is referenced
  here only as a design pattern; AA's MCP-client surface is
  Tessa-agnostic.
- Not a replacement for the Phase 1.14.A native providers
  (OpenAI, Claude, Gemini, Ollama, vLLM). Those are first-class.
  MCP servers are the *extensibility* layer for operator-specific
  services beyond the shipped provider set.
- Not federated. Per the Phase 1.14.A privacy lock, no MCP
  traffic crosses instance boundaries.

## Decision

AA adds `mcp_server` as a new provider kind in the Phase 1.14.A
AI provider abstraction. Operators register MCP servers via the
admin UI; their tools become callable through the existing AI
router, audit log, cost tracker, and privacy gate.

### Provider model

A registered MCP server is one Provider in the Phase 1.14.A
abstraction. Its `Capabilities()` enumerate the tools exposed by
that server. The router picks an MCP-server provider when:

1. A job handler or workflow explicitly invokes a specific tool
   (e.g., `ai.provider("comfyui-mcp").tool("img2img").call(...)`)
2. The operator's per-task routing config lists the MCP server
   as a fallback / preferred provider for a concern (e.g.,
   `routing.image_edit = comfyui-mcp` for the Phase 1.34
   image-editing surface)

### Registration shape

```yaml
ai.providers.comfyui-mcp:
  kind: mcp_server
  url: http://comfyui-mcp:9000/mcp        # operator-supplied
  auth:
    kind: bearer                          # or 'none', 'mtls', 'header'
    secret_ref: <secrets-vault-key>
  exposed_tools:                          # operator whitelist
    - img2img
    - upscale
    - txt2img
  capabilities_grant:                     # which AA capabilities the
    - mcp.use                             # caller needs to invoke any
    - mcp.client.images.write             # tool from this server
  health_check_interval: 60s
  rate_limit:
    per_second: 2
    per_minute: 60
```

### Per-server + per-tool capability gates

Two layers of gating, mirroring Phase 1.52's design symmetry:

1. **Per-server**: the MCP server registration declares which
   AA capabilities the caller must hold to invoke ANY tool from
   that server (operator decision; default `mcp.use`).
2. **Per-tool**: individual tools within a server can require
   additional capabilities (e.g., the `comfyui-mcp.upscale` tool
   requires `mcp.client.images.write`).

This means operators can register a sensitive MCP server (e.g.,
one that talks to a paid third-party API) and only expose its
read tools to general users while reserving write tools for
admins.

### Privacy routing

MCP servers are classified as **local** or **cloud** in
`ai.privacy.local_providers` (per the Phase 1.14.A config
schema). Restricted + embargo assets route to local MCP servers
only.

- `comfyui-mcp` running in the operator's docker compose → local
- A hosted MCP service the operator paid for → cloud

If an MCP server's classification is ambiguous (operator runs it
in their cloud), the operator declares it explicitly at
registration time. Default is `cloud` (fail-conservative).

### Audit + cost tracking

Every MCP tool invocation records to `ai_provider_call` (the
Phase 1.14.A table):

- `provider` = MCP server name (e.g., `comfyui-mcp`)
- `model` = tool name (e.g., `img2img`)
- `concern` = `complete` for generic tool calls (the existing enum
  covers it; no schema change)
- `prompt_template` / `prompt_version` = the structured tool input
  + the tool schema version the MCP server advertises
- `input_hash` / `input_tokens` / `output_tokens` / `duration_ms`
  / `estimated_cost_usd_micros` follow the same shape as other
  providers

Cost estimation is operator-configurable per (server, tool) pair
since MCP servers don't have a uniform cost model — operator
declares e.g. "comfyui-mcp.img2img costs ~$0 (local GPU time)" or
"paid-mcp.something costs $0.001 per call."

### Generic tool dispatch

MCP tool calls are dispatched via a thin `mcp.invoke(server,
tool, args)` helper. The helper:

1. Looks up the MCP server registration; verifies it's enabled +
   the tool is in the `exposed_tools` whitelist
2. Checks the per-server + per-tool capability gates against the
   caller's effective capability set
3. Routes through the Phase 1.14.A privacy gate (refuses if the
   target asset is sensitive + the server is cloud-classified)
4. Routes through the Phase 1.14.A budget gate
5. Calls the MCP server via the standard MCP protocol
   (streamable HTTP transport; stdio also supported for
   process-colocated servers)
6. Records the call to `ai_provider_call`
7. Returns the result

### Initial integration target

**ComfyUI MCP** is the first concrete consumer. The operator
runs the ComfyUI MCP server (either the bridge pattern from
SceneWeaver or any other ComfyUI MCP implementation); AA
registers it; the Phase 1.34 image-editing routing config
points the `img2img` / `inpaint` / `outpaint` / `variations`
concerns at it.

ADR 0026 (Phase 1.34) image editing was designed before this
ADR existed; its `ImageEditProvider` interface for the
`comfyui-local` provider can be implemented in two ways now:

- **Bespoke HTTP**: direct calls to ComfyUI's REST endpoints
  (the path ADR 0026 implicitly assumed)
- **MCP-mediated**: calls go through the operator's ComfyUI MCP
  bridge

ADR 0051 makes the MCP-mediated path the preferred shape — it
composes with the other operator MCP servers, gets the audit
trail for free, and stays consistent with the
"wrap-then-orchestrate" pattern. The bespoke HTTP path remains
available for operators who don't want to run an MCP bridge.

### What this is NOT (operationally)

- **Not a Tessa client.** Validated against SceneWeaver's pattern
  but contains no Tessa code or assumptions.
- **Not a generic plugin system.** MCP servers are tool *sources*,
  not plugins. Plugins (per ADR 0034) modify AA's own behavior;
  MCP servers are external services AA calls out to.
- **Not federated.** No MCP server registration crosses instance
  boundaries; no MCP traffic federates.
- **Not auto-discovered.** Operator explicitly registers each
  server. No mDNS, no auto-detection.

## Consequences

**Positive:**

- Phase 1.34 (image editing) gets a cleaner integration path for
  ComfyUI than direct HTTP.
- Studios that build internal MCP bridges (for asset validators,
  proprietary AI services, custom workflows) plug into AA
  without per-service AA code.
- The audit + cost + privacy infrastructure from Phase 1.14.A
  applies uniformly to all external tool calls, MCP or not.
- The MCP ecosystem is growing; AA being an MCP client positions
  it to consume whatever the ecosystem produces.

**Negative:**

- New surface to maintain (MCP protocol drift, server registration
  schema evolution).
- Operator burden: registering MCP servers requires URL + auth +
  tool whitelist. Mitigated by good admin UI + sensible defaults.
- Per-tool capability gates add registration complexity but the
  user's locked operator-grade security posture demands it.

## Alternatives considered

- **Bespoke HTTP integration per external service.** Rejected:
  per-service code in AA for every new operator integration
  defeats the extensibility goal.
- **Plugin system for external integrations.** Rejected: plugins
  modify AA behavior; external service integration is a different
  problem solved by MCP.
- **Wait for the MCP spec to fully stabilize.** Rejected: the
  spec is already in production use across major agent vendors;
  early-adopter cost is small vs late-adopter cost.

## Reference

- Model Context Protocol specification: https://modelcontextprotocol.io/
- ADR 0050 — Artist Alley as MCP server (sibling concept;
  opposite direction)
- ADR 0042 — Distributed catalogues, typed constants per package
  (applies to the MCP server registry + tool capability vocabulary)
- ADR 0034 — Capability add-ons (MCP servers can ship as ADR-0034
  containers operators install separately, just like `aa-ollama`
  / `aa-vllm`)
- SceneWeaver — design-pattern reference for "wrap each external
  service as its own MCP server"
- Phase 1.14.A focused brief — the AI provider abstraction this
  ADR extends
