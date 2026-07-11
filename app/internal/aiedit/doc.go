// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package aiedit is artist-alley's AI image-edit subsystem — the
// home for derived-asset operations like img2img, inpaint, outpaint,
// variations, and background removal.
//
// Lives as a sibling of [github.com/mscrnt/artist-alley/app/internal/ai]
// rather than nested under it. ai/ is infrastructure (provider
// abstraction, embeddings, transcription, the router); aiedit is
// product surface (image-edit operations the operator + artists
// interact with). Different abstractions, different lifecycles,
// different consumers. ADR 0026 names this path.
//
// # Arc shape
//
//   - 1.14.E-1 (current) — first internal MCP caller. Ships the
//     [ImageEditProvider] interface with all five ops in the
//     signature, BUT only Img2Img is implemented; the other four
//     return [ErrUnsupportedOp] so E-2 can flip them without
//     interface churn. One concrete provider:
//     [github.com/mscrnt/artist-alley/app/internal/aiedit/providers/comfyuimcp].
//     One HTTP endpoint: POST /assets/{id}/edit/img2img.
//     One job kind: aiedit.img2img.
//     One viewer trigger: the "Generate variation (AI)" button.
//
//   - 1.14.E-2 (next) — full Creative tools panel, mask drawing UI,
//     inpaint/outpaint/variations/remove-bg implementations on the
//     existing interface.
//
//   - 1.14.E-3 (later) — multi-provider tier routing gated on the
//     licensing arc; dogfood-driven polish.
//
// # Why everything routes through mcpdispatch
//
// The dispatcher's 6-step guard chain (capability → tool whitelist
// → privacy → budget → invoke → audit) IS the contract for any
// MCP call. Providers in this package wrap
// [mcpdispatch.Dispatcher.Invoke] — they never reach for the raw
// [mcp_server.Provider] directly. Bypassing the dispatcher would
// re-invent the audit + privacy + budget enforcement.
//
// # Why generated assets land via the existing upload pipeline
//
// Decode the bridge's bytes → call [storage.Service.UploadOriginal]
// → call the asset-create path normal uploads use. Thumbnailing,
// hashing, MIME detection, virus-scan hooks, audit, search indexing
// all live on that path. A side-door INSERT would create asset rows
// missing required derived state.
package aiedit
