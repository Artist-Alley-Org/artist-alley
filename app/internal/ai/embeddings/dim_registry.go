// Phase 1.14.B — model → vector-dim resolution.
//
// pgvector's HNSW index requires a fixed `vector(N)` column type, so
// embeddings persist into per-dim sibling tables
// (asset_embedding_d768, asset_embedding_d1024 — once it exists).
// The writer needs to know which table to upsert into for a given
// model. That mapping lives in system_config under the key
// `ai.embedding.dim_registry`:
//
//	{"nomic-embed-text": 768, "clip-vit-l-14": 768, ...}
//
// Holding it in system_config (not hard-coded in Go) lets operators
// add a new same-dim model without a code change. Adding a model
// with a DIFFERENT dim is a code+migration change: new
// asset_embedding_d<N> table + Go-side registration of the new dim.
//
// # Cache + invalidation
//
// The registry is a hot path (every embed write reads it). Backed
// by cache.Registry with TTL refresh + NOTIFY-driven invalidation
// when the operator edits ai.embedding.dim_registry via the admin
// surface (Phase 1.14.A wired that path already).

package embeddings

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SupportedDims enumerates every vector dim that has a corresponding
// asset_embedding_d<N> table shipped today. The writer fails fast if
// the dim_registry names a model whose dim isn't here — that surfaces
// the missing migration as a clean error instead of a silent SQL
// failure deep in the upsert.
var SupportedDims = map[int]bool{
	768: true,
	// Adding 1024 / 1536 etc.: ship the migration, then flip the bool.
}

// DimRegistry resolves model → vector dim. Thread-safe; holds the
// current snapshot in memory and re-reads from system_config on
// Invalidate().
type DimRegistry struct {
	pool *pgxpool.Pool

	mu       sync.RWMutex
	snapshot map[string]int
}

// NewDimRegistry returns an empty registry; call Refresh() before
// first use (the writer does this from its constructor).
func NewDimRegistry(pool *pgxpool.Pool) *DimRegistry {
	return &DimRegistry{pool: pool, snapshot: map[string]int{}}
}

// Refresh re-reads the system_config row. Safe to call concurrently;
// the swap is atomic. Returns the new snapshot for diagnostics.
//
// Validates that every advertised dim has a sibling table — operator
// error here means the next write would silently fail with a missing-
// table error; better to surface it at config-load time.
func (r *DimRegistry) Refresh(ctx context.Context) (map[string]int, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx,
		`SELECT value::TEXT FROM system_config WHERE key = 'ai.embedding.dim_registry'`,
	).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("embeddings: read dim_registry: %w", err)
	}

	parsed := map[string]int{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embeddings: parse dim_registry: %w", err)
	}

	for model, dim := range parsed {
		if !SupportedDims[dim] {
			return nil, fmt.Errorf(
				"embeddings: dim_registry advertises model %q with dim %d "+
					"but no asset_embedding_d%d table exists — add the migration first",
				model, dim, dim)
		}
	}

	r.mu.Lock()
	r.snapshot = parsed
	r.mu.Unlock()
	return parsed, nil
}

// DimForModel returns the vector dim configured for a model name, or
// (0, false) if the model isn't registered. Callers fall back to
// ErrUnsupportedModel when the lookup misses.
func (r *DimRegistry) DimForModel(model string) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	dim, ok := r.snapshot[model]
	return dim, ok
}

// Snapshot returns a copy of the current map. Used by the admin
// surface to render "which models are registered" without exposing
// the internal mutex.
func (r *DimRegistry) Snapshot() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.snapshot))
	for k, v := range r.snapshot {
		out[k] = v
	}
	return out
}
