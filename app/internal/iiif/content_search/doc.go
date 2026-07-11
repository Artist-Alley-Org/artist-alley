// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package content_search serves IIIF Content Search 2.0 responses
// against the local corpus. Phase 1.54.B.
//
// Endpoint shape (mounted alongside the Presentation API):
//
//	GET /iiif/3/asset/{id}/search?q=<term>
//	GET /iiif/3/collection/{id}/search?q=<term>
//
// Response: an AnnotationPage per IIIF Content Search 2.0 spec:
//
//	{
//	  "@context": "http://iiif.io/api/search/2/context.json",
//	  "id": "...", "type": "AnnotationPage",
//	  "items": [{
//	    "id": "...", "type": "Annotation",
//	    "motivation": "supplementing",
//	    "body": { "type": "TextualBody", "value": "...", "format": "text/plain" },
//	    "target": "<canvas-id>"
//	  }, ...]
//	}
//
// Two dispatch paths:
//
//   - Asset-scope: search runs LOCALLY against the asset's own
//     metadata pairs (loader-provided). No corpus fan-out. Simple
//     substring match with case-folded compare — Postgres FTS is
//     overkill for a 50-row set + gives us TextGranularity=line
//     naturally. Every matching metadata pair emits ONE Annotation
//     targeting the asset's canvas/1.
//
//   - Collection-scope: dispatches through search.Engine with the
//     query text + Type filter=asset, then filters the result set
//     to only those hits whose asset ID is a member of the target
//     collection. Each surviving hit emits ONE Annotation targeting
//     the hit's member manifest ID. This mirrors how Mirador expects
//     search results to link back to member canvases.
//
// The endpoint is IIIF-only — the frontend/site search UI still
// consumes /search directly per 1.16.B. Both share the same Engine
// so relevance is consistent.
package content_search
