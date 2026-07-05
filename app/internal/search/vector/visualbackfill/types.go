package visualbackfill

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Scope describes which image-asset population a run walks. MVP
// supports only ScopeAll ("every image asset lacking a visual
// embedding"); the JSONB column matches reindex.Scope so the admin
// history table renders identically once further kinds land.
type Scope struct {
	Kind ScopeKind `json:"kind"`
}

// ScopeKind enumerates the supported scope filters.
type ScopeKind string

const (
	// ScopeAll matches every image asset that lacks a visual
	// embedding. Backing query: visualstore.ListImageAssetsNeedingVisualEmbedding.
	ScopeAll ScopeKind = "all"
)

// Row is the plain-Go projection of one search_visual_backfill_run
// row. Mirrors reindex.Row minus the target column.
type Row struct {
	ID               uuid.UUID
	StartedAt        time.Time
	CompletedAt      *time.Time
	CancelledAt      *time.Time
	Scope            Scope
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
	Scope          Scope
	TotalEstimated *int64
	StartedBy      *int64
}

// Sentinels — HTTP layer maps to appropriate status codes.
var (
	// ErrActiveRunExists is returned by Store.Start when a run is
	// already in-progress. HTTP maps to 409.
	ErrActiveRunExists = errors.New("visualbackfill: an active run already exists")

	// ErrNotFound is returned by Store.Get / Cancel when the id
	// doesn't exist. HTTP maps to 404.
	ErrNotFound = errors.New("visualbackfill: run not found")

	// ErrProviderUnavailable is returned by the HTTP handler when
	// the visual provider isn't registered (sidecar not enabled OR
	// unreachable at boot). HTTP maps to 503 with a diagnostic body
	// so the operator knows to check sysconfig + sidecar health.
	ErrProviderUnavailable = errors.New("visualbackfill: visual provider not registered")
)
