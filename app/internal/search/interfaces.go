// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
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
	// Text is the free-text search query, and it is OPTIONAL.
	//
	// ⚠️ This comment used to read "empty text is rejected upstream —
	// /search requires a query", and it has been wrong since #1157. What
	// counts as a search is decided in ONE place, [Engine.Run], because
	// that is the only place that can see all three of text, similarity
	// hint and facet selection; the upstream copy at the HTTP edge was
	// DELETED rather than widened, precisely so the two could not come to
	// disagree (ADR 0070 / #1023). A query carrying a filter and no text
	// is a complete question — "everything at pipeline stage Final" is
	// the primary thing an advanced search page exists to ask — and it
	// returns a normal result body. Only a request with none of the three
	// gets ErrEmptyQuery.
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

	// Caps is the caller's content-plane capabilities, resolved at
	// the HTTP edge (#899). Before this the handler read
	// id.UserRef and dropped id.Capabilities on the floor, so the
	// engine could not tell a demo-viewer holding
	// `content.read.all` from a stranger — which is why the
	// sensitivity rule could not be applied here at all.
	//
	// A resolved struct rather than a visibility.CapabilityChecker
	// closure because this value has to reach the CACHE KEY, and a
	// closure has no stable encoding. Zero value = no
	// capabilities, which is the correct default for anonymous.
	Caps visibility.ContentCaps

	// PostCaps is the caller's post-plane capabilities, resolved at the
	// same edge (#873). The post read rule's `private` tier opens for
	// posts.admin / system.admin, and until search composed the whole
	// rule there was nothing here to open it with.
	//
	// Separate from Caps rather than folded into it because they gate
	// different planes: ContentCaps decides whether asset BYTES (and so
	// asset fields) reach the caller, this decides which post ROWS
	// exist. Both fold into the cache key.
	PostCaps visibility.PostCaps

	// MutationCaps is the caller's `assets.admin` scope, resolved at the
	// same edge (#939, ADR 0064). A holder is owed the FIELDS of the
	// assets they may edit — otherwise they administer titles they
	// cannot see — and is still refused the picture and the bytes.
	//
	// A third resolved struct rather than two more booleans on Caps
	// because this capability is TEAM-SCOPED: the honest resolved form
	// is a set of team IDs, and ContentCaps' contract is two global
	// booleans and a two-byte cache key. It folds into the key for the
	// same revoke-direction reason the other two do; see
	// visibility.AssetMutationCaps.CacheKey.
	MutationCaps visibility.AssetMutationCaps

	// Mature is the caller's resolved mature-content axis (#1117,
	// ADR 0090). Three booleans — signed in, opted in, instance allows
	// — resolved once at the HTTP edge and carried, exactly like the
	// three caps structs above and for the same reasons: the inputs come
	// from three different stores, and the answer has to reach both a
	// SQL fragment and the CACHE KEY.
	//
	// ⚠️ THE ZERO VALUE IS THE DISQUALIFIED VIEWER. A Query built without
	// this field searches as an opted-out reader does, which returns
	// FEWER rows. That direction is deliberate (visibility.MatureViewer):
	// a gate that loses its inputs must refuse rather than widen, and the
	// visible symptom is a reader saying "I opted in and still cannot
	// find it" rather than an invisible leak.
	//
	// ⚠️ AND IT MUST BE IN THE CACHE KEY. Without that, an opted-in
	// reader's cached result page is served verbatim to an opted-out one
	// — a leak no single-caller test can see, because it needs two
	// callers and a warm cache to exist at all. See
	// visibility.MatureViewer.CacheKey and keyForQuery.
	Mature visibility.MatureViewer
	// CapChecker is the caller's RAW capability lookup (#1157).
	//
	// Its three neighbours above are resolved VALUE types, and that is
	// the right shape for them: each answers a fixed, small set of
	// questions the engine knows at compile time, so resolving at the
	// edge makes them cache-key-able and keeps the engine from holding a
	// live identity. `field_definition.read_capability` is not that
	// shape. A capability code is DATA an operator types into a field
	// definition at runtime, so the set of questions is open and there
	// is nothing to resolve into.
	//
	// [visibility.ContentCaps.Checker] cannot stand in: it answers
	// `system.admin` and `content.read.all` and returns false for every
	// other code, so using it for the field gate would refuse a
	// capability-gated field to the very holder of its capability —
	// fail-closed, and a broken feature.
	//
	// Same reasoning and same shape as suggest.Request.CollectionCaps
	// (#1078), which is a raw checker for the same reason: the rule it
	// feeds takes one. Nil = no capabilities, correct for anonymous.
	//
	// ⚠️ IT CANNOT BE A CACHE-KEY COMPONENT, and that has a consequence
	// recorded on [Service.Execute]: an open set of capability codes has
	// no finite key, and `keyForQuery` is a pure function that cannot
	// query the database to learn which of them this caller holds. The
	// cache is consulted BEFORE Engine.Run, so [facet.Selection.Authorize]
	// — which is inside Run — cannot protect a cache hit. A search whose
	// selection names a `field:` term therefore bypasses the cache
	// entirely. See [facet.Selection.NamesFieldDimension].
	CapChecker visibility.CapabilityChecker

	// Filters is the caller's facet selection — the tag, asset type,
	// owner, sensitivity or extension they narrowed to (#907).
	//
	// This field replaces the `Advanced *AdvancedQuery` placeholder that
	// sat here through five releases saying "nil in B-1; the engine
	// ignores it". It was true: the DSL compiled a Filters struct, the
	// aggregators counted every bucket correctly, and nothing anywhere
	// applied one, so ticking a facet had never once changed a result
	// set. The placeholder is gone rather than kept beside the real
	// field — a struct with both would leave the next reader guessing
	// which one the engine reads.
	//
	// Populated from TWO sources that produce the same type: the
	// repeated `filter=` query parameter (the rail) and the compiled
	// DSL's field:value nodes (the typed query). Both compose; neither
	// is privileged.
	//
	// An entity that cannot satisfy the selection contributes no hits
	// and no count — see [facet.Selection.SQL].
	Filters facet.Selection

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
	//
	// #899 note: "for permission checks in downstream consumers"
	// described an intention, not a caller — nothing consumed it,
	// and the permission check it was there for never got written,
	// which is how the asset title and description below shipped
	// ungated for three releases. The check now happens HERE, at
	// projection time, where the sensitivity columns are in scope;
	// this field is a display value and not a hook. It is NOT
	// carried on a withheld hit — see Restricted.
	OwnerUserRef *int64

	// Restricted is true when the caller fails
	// visibility.FieldsReadable for this hit's asset (#899): they
	// may see that the row exists but not its columns. Title,
	// Summary, ExtraJSON (which carries the thumbhash — a blurred
	// picture of the content), OwnerUserRef and the timestamps are
	// then all ABSENT from the wire, and OwnerDisplayName is the
	// only asset-derived value that survives.
	//
	// Decided in the projection, never in MarshalHitJSON: the
	// marshaller has no caller in scope, and a security decision
	// that cannot see its subject is a security decision waiting
	// to be wrong.
	Restricted bool

	// OwnerDisplayName is the asset owner's display name per
	// visibility.OwnerDisplayNameSQL, carried ONLY on a restricted
	// hit so the placeholder card can say whose work it is and #881
	// can address the request. Empty when unresolvable and when the
	// owner opted out of anonymous exposure (#1023), and then
	// omitted from the wire rather than sent empty.
	OwnerDisplayName string

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
