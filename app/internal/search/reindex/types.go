package reindex

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Target enumerates what the reindex writes. Multi-value: a single
// run can rebuild tsvectors AND enqueue embeds in one pass.
type Target string

const (
	TargetTsvector  Target = "tsvector"
	TargetEmbedding Target = "embedding"
	TargetBoth      Target = "both"
)

// ValidTarget reports whether s is a supported target.
func ValidTarget(s string) bool {
	switch Target(s) {
	case TargetTsvector, TargetEmbedding, TargetBoth:
		return true
	}
	return false
}

// ScopeKind identifies which asset population a run walks.
type ScopeKind string

const (
	ScopeAll                ScopeKind = "all"
	ScopeAssetType          ScopeKind = "asset_type"
	ScopeCollection         ScopeKind = "collection"
	ScopeEmbeddingModel     ScopeKind = "embedding_model"
	ScopeFederationMissing  ScopeKind = "federation_missing"
)

// Scope carries the parsed scope filter. Stored on the run row as
// JSONB so the admin UI can render it verbatim in the history
// table without an extra columns migration per scope shape.
type Scope struct {
	Kind ScopeKind `json:"kind"`

	// AssetTypeID scopes to assets under one asset_type row.
	AssetTypeID *uuid.UUID `json:"asset_type_id,omitempty"`

	// CollectionID scopes to assets carrying that collection's ref.
	CollectionID *uuid.UUID `json:"collection_id,omitempty"`

	// EmbedProvider + EmbedModel filter existing rows in
	// asset_embedding_d768 by (provider, model). Empty model
	// matches every model for that provider.
	EmbedProvider string `json:"embed_provider,omitempty"`
	EmbedModel    string `json:"embed_model,omitempty"`
}

// ParseScope parses the operator-facing "<kind>:<selector>" wire
// form into a typed Scope. Empty input defaults to ScopeAll.
//
// Grammar:
//
//	all
//	asset_type:<uuid>
//	collection:<uuid>
//	embedding_model:<provider>/<model>     // model optional
//	federation_missing
func ParseScope(raw string) (Scope, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return Scope{Kind: ScopeAll}, nil
	}
	if raw == "federation_missing" {
		return Scope{Kind: ScopeFederationMissing}, nil
	}
	kind, selector, ok := strings.Cut(raw, ":")
	if !ok {
		return Scope{}, fmt.Errorf("%w: expected <kind>:<selector>", ErrBadScope)
	}
	switch ScopeKind(kind) {
	case ScopeAssetType:
		id, err := uuid.Parse(strings.TrimSpace(selector))
		if err != nil {
			return Scope{}, fmt.Errorf("%w: asset_type: %v", ErrBadScope, err)
		}
		return Scope{Kind: ScopeAssetType, AssetTypeID: &id}, nil
	case ScopeCollection:
		id, err := uuid.Parse(strings.TrimSpace(selector))
		if err != nil {
			return Scope{}, fmt.Errorf("%w: collection: %v", ErrBadScope, err)
		}
		return Scope{Kind: ScopeCollection, CollectionID: &id}, nil
	case ScopeEmbeddingModel:
		provider, model, _ := strings.Cut(strings.TrimSpace(selector), "/")
		if provider == "" {
			return Scope{}, fmt.Errorf("%w: embedding_model requires <provider>[/<model>]", ErrBadScope)
		}
		return Scope{Kind: ScopeEmbeddingModel, EmbedProvider: provider, EmbedModel: model}, nil
	}
	return Scope{}, fmt.Errorf("%w: unknown kind %q", ErrBadScope, kind)
}

// Row is the plain-Go projection of one search_reindex_run row.
type Row struct {
	ID               uuid.UUID
	StartedAt        time.Time
	CompletedAt      *time.Time
	CancelledAt      *time.Time
	Scope            Scope
	Target           Target
	TotalEstimated   *int64
	Processed        int64
	Succeeded        int64
	Failed           int64
	StartedByUserRef *int64
	LastError        *string
}

// IsActive reports whether the run is still in-progress.
func (r Row) IsActive() bool {
	return r.CompletedAt == nil && r.CancelledAt == nil
}

// StartParams is the operator-facing input to Store.Start.
type StartParams struct {
	Scope     Scope
	Target    Target
	StartedBy *int64
}

// Sentinels — HTTP layer maps to appropriate status codes.
var (
	// ErrBadScope is returned by ParseScope for invalid wire input.
	// HTTP maps to 400.
	ErrBadScope = errors.New("reindex: invalid scope")

	// ErrActiveRunExists is returned by Store.Start when a run is
	// already in-progress (the partial UNIQUE INDEX enforces this
	// at the DB layer; the store surfaces it as a typed error).
	// HTTP maps to 409.
	ErrActiveRunExists = errors.New("reindex: an active run already exists")

	// ErrNotFound is returned by Store.Get / Cancel when the id
	// doesn't exist. HTTP maps to 404.
	ErrNotFound = errors.New("reindex: run not found")
)
