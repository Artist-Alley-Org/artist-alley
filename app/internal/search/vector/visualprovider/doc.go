// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package visualprovider is the Phase 1.16.B-3-followup abstraction
// for the aa-clip-visual-local sidecar. Closes #183.
//
// The sidecar is optional: operators who don't install it get the
// pre-existing 501 sidecar_not_installed response on POST
// /search/by-image. Operators who install + configure it get
// reverse-image search backed by CLIP ViT-L/14 visual embeddings
// stored in the new asset_visual_embedding table.
//
// Two embedding spaces coexist without cross-contamination:
//
//   - Text embeddings (asset_embedding_d768 via Ollama
//     nomic-embed-text) power the existing DSL similar_to:<uuid>
//     path + hybrid text+vector ranking from 1.16.B-3.
//   - Visual embeddings (asset_visual_embedding via this sidecar)
//     power POST /search/by-image ONLY.
//
// Queries against either table use the appropriate query vector
// (text-derived for text queries, CLIP-derived for image uploads).
// Cosine similarity between the two spaces is meaningless — the
// package structure enforces that separation.
//
// Boot registration:
//
//   - sysconfig.search.visual.enabled = false (default): provider
//     is NOT registered; POST /search/by-image continues to return
//     501.
//   - sysconfig.search.visual.enabled = true: Server.Run polls
//     the sidecar's /health endpoint; on success, registers a
//     LocalProvider on the search subsystem; the by-image handler
//     switches to the 200 code path.
//   - Sidecar unreachable at boot even with enabled=true: the
//     provider is NOT registered; 501 is still the response. Boot
//     logs a warn once so operators can diagnose via /admin/search/health.
package visualprovider
