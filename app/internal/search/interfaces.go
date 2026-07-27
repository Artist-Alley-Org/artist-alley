// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"time"

	"github.com/google/uuid"
)

// HitType identifies which entity a search hit represents.
type HitType string

const (
	HitTypeAsset      HitType = "asset"
	HitTypeCollection HitType = "collection"
	HitTypePost       HitType = "post"
)

// AllHitTypes returns every supported HitType in a stable order.
// Used by the endpoint to normalise a missing / "*" types filter to
// the full set.
func AllHitTypes() []HitType {
	return []HitType{HitTypeAsset, HitTypeCollection, HitTypePost}
}

// ParseHitType returns the canonical HitType for a case-insensitive
// input string; ok=false on unknown input.
func ParseHitType(s string) (HitType, bool) {
	switch s {
	case "asset", "assets":
		return HitTypeAsset, true
	case "collection", "collections":
		return HitTypeCollection, true
	case "post", "posts":
		return HitTypePost, true
	}
	return "", false
}

// Query is the parsed, normalised input to the unified search
// engine. The HTTP handler builds one from the request query
// string; tests construct freely.
type Query struct {
	// Text is the free-text search query. Empty text is rejected
	// upstream — /search requires a query.
	Text string

	// Types is the set of entity kinds to search. Empty slice
	// treated as "all supported types" by the engine.
	Types []HitType

	// Limit is the max number of hits to return in this page.
	// Handler caps at 100.
	Limit int

	// Cursor is the pagination cursor from a previous response's
	// NextCursor. Nil = first page.
	Cursor *Cursor

	// CallerUserRef is the authenticated caller's user_ref, or nil
	// for anonymous requests. Used both for visibility gating and
	// as a component of the cache key so User A's cache never
	// serves User B.
	CallerUserRef *int64

	// Advanced is a placeholder for the B-2 advanced DSL
	// (field:value, phrases, AND/OR/NOT). Nil in B-1; the engine
	// ignores it. Kept here so the outer shape stays stable
	// when the DSL parser lands.
	Advanced *AdvancedQuery

	// SimilarityHint is the pgvector-formatted embedding literal
	// ('[a,b,c,...]') the Engine's hybrid path treats as the
	// anchor for kNN retrieval. Non-empty when the caller wrote
	// similar_to:<uuid> in the DSL or hit the reserved
	// /search/by-image endpoint (once its encoder ships). Nil in
	// pure-BM25 queries.
	//
	// Phase 1.16.B-3 addition.
	SimilarityHint string

	// SimilarityHintProvider / Model / Modality carry the tuple
	// the anchor embedding was stored under. Populated by the
	// Service when resolving similar_to:<uuid>; filters kNN
	// candidates to the same vector space (cross-space cosine
	// similarity is meaningless).
	SimilarityHintProvider string
	SimilarityHintModel    string
	SimilarityHintModality string

	// SimilarityHintID is a stable identifier for the hint so
	// SearchCache can key against it (asset:<uuid> for
	// similar_to; image:<sha256> for by-image; empty for
	// pure-BM25). Distinct from SimilarityHint so the cache key
	// stays small.
	SimilarityHintID string

	// HybridWeight ∈ [0,1] controls how BM25 and vector scores
	// combine. 0 = pure BM25 (B-1/B-2 behaviour); 1 = pure
	// vector; 0.5 = default hybrid. The Engine's hybrid path
	// computes hybrid_score = (1-w)*bm25 + w*cosine_sim.
	//
	// Ignored when SimilarityHint is empty (BM25-only path).
	HybridWeight float64

	// SimilarityThreshold filters vector hits below this cosine
	// similarity value ∈ [0,1]. Zero disables the filter (default
	// per Engine constants). Applied AFTER the hybrid score
	// merge; a pure-BM25 query bypasses the threshold entirely.
	SimilarityThreshold float64
}

// AdvancedQuery is the B-2 placeholder. B-1 never populates it.
type AdvancedQuery struct{}

// Cursor is the opaque pagination cursor. Serialised to base64-
// encoded JSON before it crosses the wire so clients treat it as
// opaque.
type Cursor struct {
	// LastScore is the normalised score of the last hit on the
	// previous page — the next page starts at rows scoring at
	// most this value.
	LastScore float64 `json:"s"`

	// LastID is the UUID of the last hit on the previous page.
	// Tie-breaker for rows scoring identically.
	LastID uuid.UUID `json:"i"`

	// LastType is the HitType of the last hit on the previous
	// page. Third tie-breaker so ordering is total across mixed
	// entity types.
	LastType HitType `json:"t"`
}

// Hit is a single search result — one row from one of the three
// entities, projected to the shared summary shape the frontend
// needs to render a card.
type Hit struct {
	Type HitType
	ID   uuid.UUID

	// Title is the entity's display title (asset.title,
	// collection.name, post.title).
	Title string

	// Summary is the entity's short-form body (asset.description
	// truncated, collection.description, post.description).
	Summary string

	// OwnerUserRef is the row's owner (for permission checks in
	// downstream consumers). Nil when the entity has no owner
	// concept (currently none — populated on all three).
	OwnerUserRef *int64

	// OriginServerID is set for federated rows so the frontend
	// can badge them.
	OriginServerID *uuid.UUID

	// CreatedAt / UpdatedAt let the frontend show recency without
	// a per-hit follow-up fetch.
	CreatedAt time.Time
	UpdatedAt time.Time

	// RawScore is the ts_rank_cd value the entity's query
	// returned. Kept for debug/logging; not surfaced to clients.
	RawScore float64

	// NormalisedScore is [0,1] within the QueryResult — computed
	// from RawScore / (per-entity max score). Cross-entity
	// ordering happens on this field.
	NormalisedScore float64

	// VectorScore is the cosine similarity ∈ [0,1] the hit
	// received from the pgvector kNN pass. Zero for BM25-only
	// hits (query had no SimilarityHint) OR for hits present in
	// BM25 results but missing from the vector top-K.
	//
	// Phase 1.16.B-3 addition; populated by the hybrid Engine
	// path.
	VectorScore float64

	// HybridScore is the weighted merge of NormalisedScore +
	// VectorScore per Query.HybridWeight. Populated only when
	// the Engine ran the hybrid path; equal to NormalisedScore
	// for BM25-only queries.
	HybridScore float64

	// ExtraJSON is the per-entity extras the frontend uses to
	// render an entity-specific card (asset thumbhash, post
	// cover_asset_id). Stored as raw JSON so the OpenAPI
	// additionalProperties shape is honoured. Nil is legal and
	// marshals to `{}` — collection hits carry no extras since
	// #650 dropped the stale `featured` flag (ADR 0065).
	ExtraJSON []byte
}

// QueryResult is the unified response the engine returns to the
// HTTP handler.
type QueryResult struct {
	Hits             []Hit
	NextCursor       *Cursor
	TotalCount       int
	TotalCountCapped bool
	TypesMatched     []HitType

	// Facets is the B-2 placeholder for facet buckets. Empty in
	// B-1 so the outer shape doesn't change when facets land.
	Facets []FacetBucket
}

// FacetBucket is the B-2 placeholder shape. Empty in B-1.
type FacetBucket struct{}
