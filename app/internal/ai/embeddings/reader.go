// Phase 1.14.B — similarity-search read side.
//
// The Writer above persists embeddings; Reader fetches them back +
// runs kNN over the per-dim sibling table. Operators wire both
// behind the same package so the dim_registry lookup is shared.

package embeddings

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAnchorHasNoEmbedding is returned when the caller asks for
// similars to an asset that hasn't been embedded yet. The HTTP
// handler maps this to `{anchor_has_embedding: false, results: []}`
// so the UI can render "embedding pending" instead of erroring.
var ErrAnchorHasNoEmbedding = errors.New("embeddings: anchor asset has no embedding")

// Neighbour is one (asset_id, distance) row from a kNN query.
// The HTTP handler joins this with the assets table to produce the
// public SimilarAsset response shape.
type Neighbour struct {
	AssetID  uuid.UUID
	Distance float64
}

// Reader runs read-side queries against the per-dim sibling tables.
// Shares the dim_registry with Writer — same instance, same TTL
// snapshot — so a model bump that swaps tables is visible to both
// without re-construction.
type Reader struct {
	pool *pgxpool.Pool
	dims *DimRegistry
}

// NewReader binds a reader to a pool + dim registry. Caller typically
// passes the registry the Writer already holds.
func NewReader(pool *pgxpool.Pool, dims *DimRegistry) *Reader {
	return &Reader{pool: pool, dims: dims}
}

// HasEmbedding reports whether the anchor has a stored embedding for
// the requested (provider, model, modality). The HTTP handler uses
// this to disambiguate "no neighbours" from "embedding pending".
func (r *Reader) HasEmbedding(ctx context.Context, anchorID uuid.UUID, provider, model, modality string) (bool, error) {
	dim, ok := r.dims.DimForModel(model)
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrUnsupportedModel, model)
	}
	q := New(r.pool)
	switch dim {
	case 768:
		return q.AssetEmbeddingExistsD768(ctx, AssetEmbeddingExistsD768Params{
			AssetID:  pgtype.UUID{Bytes: anchorID, Valid: true},
			Provider: provider,
			Model:    model,
			Modality: modality,
		})
	default:
		return false, fmt.Errorf("embeddings.HasEmbedding: dim %d registered for model %q but no read case wired", dim, model)
	}
}

// FindSimilarByAnchor returns up to `limit` neighbours of the anchor
// asset ranked by ascending cosine distance. The anchor itself is
// excluded by the SQL. `model` + `modality` define the search space —
// different models live in different vector spaces; cross-model
// cosine is meaningless.
//
// Returns ErrAnchorHasNoEmbedding when the anchor has no embedding
// row for the requested (provider, model, modality) tuple. Returns
// an empty slice + nil error when the anchor exists but no other
// asset has been embedded yet.
func (r *Reader) FindSimilarByAnchor(
	ctx context.Context,
	anchorID uuid.UUID,
	provider, model, modality string,
	limit int,
) ([]Neighbour, error) {
	// Cheap existence probe so we can distinguish "no neighbours"
	// from "embedding pending" without scanning the whole table on
	// the latter.
	hasAnchor, err := r.HasEmbedding(ctx, anchorID, provider, model, modality)
	if err != nil {
		return nil, err
	}
	if !hasAnchor {
		return nil, ErrAnchorHasNoEmbedding
	}

	dim, _ := r.dims.DimForModel(model)
	q := New(r.pool)

	switch dim {
	case 768:
		rows, err := q.FindSimilarAssetsByAnchorD768(ctx, FindSimilarAssetsByAnchorD768Params{
			Provider:       provider,
			Model:          model,
			Modality:       modality,
			AnchorAssetID:  pgtype.UUID{Bytes: anchorID, Valid: true},
			ResultLimit:    int32(limit),
		})
		if err != nil {
			return nil, fmt.Errorf("embeddings.FindSimilarByAnchor: knn: %w", err)
		}

		out := make([]Neighbour, 0, len(rows))
		for _, n := range rows {
			if !n.AssetID.Valid {
				continue
			}
			out = append(out, Neighbour{
				AssetID:  uuid.UUID(n.AssetID.Bytes),
				Distance: distanceAsFloat(n.Distance),
			})
		}
		return out, nil

	default:
		return nil, fmt.Errorf("embeddings.FindSimilarByAnchor: dim %d registered for model %q but no read case wired", dim, model)
	}
}

// distanceAsFloat unboxes sqlc's interface{} return for the cosine
// expression. pgx scans a NUMERIC into different concrete types
// depending on driver version (float64, string, pgvector type) —
// the cases below cover what we've seen in practice.
func distanceAsFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case pgtype.Numeric:
		f, _ := x.Float64Value()
		return f.Float64
	}
	return 0
}
