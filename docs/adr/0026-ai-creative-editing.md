---
id: "0026"
title: AI creative editing — in-paint, out-paint, variations
status: accepted
date: 2026-05-30
area: extensibility
phases: 
  - "1.20"
  - "1.22"
  - "1.23"
  - "1.34"
  - "1.14.E"
supersedes: []
related: 
  - "0017"
tags:
  - extensibility
  - ai
  - infrastructure
  - 3d
excerpt: >-
  The DALL-E erase + regenerate pattern is widely used for marketing asset prep (remove background, regenerate a logo placement, generate variations for A / B testing), concept-art ideation (mask part of a sketch and regenerate it), and reference-image cleanup.
---
## Status (2026-06-22)

First op shipped via PR #156 (Phase 1.14.E-1): `img2img` against an operator-run ComfyUI MCP bridge through the ADR 0051 MCP-mediated path. The ComfyUI provider is implemented as an MCP client (`app/internal/aiedit/providers/comfyuimcp/`), not as a bespoke HTTP integration — the MCP composition gets the audit + cost + privacy infrastructure from Phase 1.14.A for free and stays consistent with the wider operator tool surface. Source-of-truth preserved: generated images land as new assets linked to the source via the `creative_lineage` table (migration 00014), exactly per the "output as new asset, not replacement" rule below.

Remaining (1.14.E-2 / 1.14.E-3): the other four ops (inpaint / outpaint / variations / remove-bg), the full Creative tools panel with mask drawing UI, OpenAI + Stability hosted providers, and tier-aware provider gating (Community vs Pro vs Enterprise per the per-tier table below).

## Context

The DALL-E erase + regenerate pattern is widely used for marketing
asset prep (remove background, regenerate a logo placement, generate
variations for A / B testing), concept-art ideation (mask part of
a sketch and regenerate it), and reference-image cleanup. The audit
(2026-05-30) flagged this as Phase 1.20+ in the lower-priority bucket.
User locked it in 2026-05-30.

## Decision

Add Phase 1.34 — AI creative editing — as a provider-abstracted layer
that exposes in-paint, out-paint, and variation generation in the
asset viewer, with the heavy compute deferred to whichever provider
the operator configures.

### Provider abstraction

```go
type ImageEditProvider interface {
    Name() string                                        // "openai", "stability", "comfyui-local", ...
    Capabilities() Capabilities                          // {InPaint, OutPaint, Variations, RemoveBg}
    InPaint(ctx, src, mask, prompt, opts) (asset, error)
    OutPaint(ctx, src, direction, prompt, opts) (asset, error)
    Variations(ctx, src, count, opts) ([]asset, error)
    RemoveBackground(ctx, src) (asset, error)
}
```

Provider packages live in `app/internal/aiedit/<name>/`.

### Initial providers

- **OpenAI** (DALL-E + GPT-Image) — the easiest on-ramp, hosted.
- **Stability AI** — second hosted option, different aesthetic.
- **ComfyUI local** — for studios who run their own GPUs and don't
  want to send pixels off-network. Implemented as an MCP client per
  ADR 0051: operator runs the ComfyUI MCP bridge (ships at
  `tools/comfyui-mcp-bridge/`) alongside ComfyUI; AA registers the
  bridge and dispatches through `mcpdispatch.Dispatcher.Invoke`. The
  bespoke-HTTP path remains available for operators who don't want
  to run an MCP bridge but is not the recommended shape.

### Viewer integration

In the asset viewer, a "Creative tools" panel appears for image
assets (per-image; never for video or 3D in v1). Tools:

- **Mask + regenerate** — brush over an area on the canvas, type a
  prompt, get N candidates back to choose from.
- **Out-paint** — extend the image in a chosen direction by a
  configurable percentage.
- **Variations** — generate N similar but different versions.
- **Remove background** — one-click for a transparent-background
  output.

### Output as new asset, not replacement

Generated images are new assets that share a `creative_lineage`
relationship with the source. The source is never overwritten. The
new asset's metadata records the provider, the prompt, the seed (if
available), and the parameters used. This is critical for both
licensing (the provider's terms may attach) and for audit.

### Per-tier behaviour

- **Community:** ComfyUI local only (the studio brings their own
  GPU; no Artist-Alley-paid AI compute).
- **Pro:** ComfyUI local + OpenAI + Stability with per-month token
  budgets per license.
- **Enterprise:** unlimited provider + bring-your-own API key for
  the hosted providers.

Enforced via the tangled-derivation model from ADR 0017.

### Provider terms surface

Each provider has its own content policy and license. The viewer
surfaces the provider's terms link before generating; outputs carry
a `provider_terms_acknowledged_at` audit entry.

## Consequences

**Positive**

- Brings a real marketing-grade tool into the review surface without
  a separate app.
- The "output as new asset" rule preserves the source-of-truth
  forever; no destructive AI edits.
- Provider abstraction means studios who require local-only compute
  (ComfyUI) get the same UX as cloud-AI customers.

**Negative**

- Provider terms change frequently and the surface needs to stay
  current. Mitigation: terms link is a configurable field per
  provider; ops can update without a release.
- AI-generated content's IP status is jurisdiction-dependent and
  evolving. Mitigation: the `creative_lineage` relationship +
  prompt audit are exactly what a legal review needs to assess.

## Alternatives considered

- **OpenAI only.** Misses local-only and Stability customers.
- **Destructive edits in-place.** Rejected — too easy to lose the
  source asset.
- **Ship as a plugin only.** Plugin ecosystem is later (Phase 1.23);
  this is high-leverage enough to land natively.

## Reference

- Phase 1.34 in [`docs/roadmap.md`](../roadmap.md).
- Federation (Phase 1.22) — `creative_lineage` rows replicate across
  peers via the same outbox mechanism.
