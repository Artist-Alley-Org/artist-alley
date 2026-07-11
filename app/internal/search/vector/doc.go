// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package vector is the vector-search layer added in Phase 1.16.B-3.
// Consumed by the search Engine's hybrid path + the DSL's similar_to
// compilation + the reserved /search/by-image endpoint.
//
// Two entry points:
//
//   - [Fetcher.FetchAssetEmbedding] resolves an asset UUID to the
//     stored 768-dim vector. Used by the DSL compiler when the user
//     writes similar_to:<uuid>.
//   - [Query] runs a kNN search against asset_embedding_d768,
//     joined against the shared visibility.Predicate so restricted
//     assets never appear in results for callers who can't see them.
//
// This package deliberately does NOT implement image encoding —
// the current AI subsystem provides text embeddings via
// nomic-embed-text (through the cliplocal provider, which wraps
// Ollama). A real CLIP image encoder is a separate follow-up per
// pre-audit Q3 findings; POST /search/by-image is reserved as a
// 501 handler until that sidecar ships.
//
// Query text embeddings share the SAME encoder used for asset
// embeddings (per system_config ai.routing.embed → cliplocal →
// nomic-embed-text). That keeps query text and asset text in the
// same 768-dim space; ranking is meaningful. Documented divergence
// from the B-3 brief: no separate CLIP text-encoder sidecar is
// shipped in this PR — the existing embedding path is used.
package vector
