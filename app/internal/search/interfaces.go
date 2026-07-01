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

	// ExtraJSON is the per-entity extras the frontend uses to
	// render an entity-specific card (asset thumbhash, post
	// cover_asset_id, collection featured flag). Stored as raw
	// JSON so the OpenAPI additionalProperties shape is honoured.
	ExtraJSON []byte
}

// QueryResult is the unified response the engine returns to the
// HTTP handler.
type QueryResult struct {
	Hits            []Hit
	NextCursor      *Cursor
	TotalCount      int
	TotalCountCapped bool
	TypesMatched    []HitType

	// Facets is the B-2 placeholder for facet buckets. Empty in
	// B-1 so the outer shape doesn't change when facets land.
	Facets []FacetBucket
}

// FacetBucket is the B-2 placeholder shape. Empty in B-1.
type FacetBucket struct{}
