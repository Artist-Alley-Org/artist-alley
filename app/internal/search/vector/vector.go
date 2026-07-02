package vector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Dim is the CLIP / embedding vector dimensionality this package
// operates against. Matches the schema's asset_embedding_d768 table
// + the dim_registry entry seeded in migration 00011.
const Dim = 768

// DefaultProvider is the fallback provider value used to look up
// stored embeddings when the caller hasn't specified one. Mirrors
// the seed default in system_config.ai.routing.embed shipped by
// migration 00009.
const DefaultProvider = "clip_local"

// DefaultModel is the fallback model name. Aligns with the current
// Ollama-nomic-embed-text encoder used at ingest.
const DefaultModel = "nomic-embed-text"

// DefaultModality is the modality label the ingest job writes for
// text-derived embeddings. The asset_embedding_d768 primary key
// includes (asset_id, provider, model, modality), so the search
// path must match what ingest wrote.
const DefaultModality = "text"

// Hit is one vector-similarity result. Score is the cosine
// similarity — the pgvector <=> operator returns cosine DISTANCE
// (0 = identical, 2 = opposite for unit vectors); this package
// converts to similarity as 1 - distance so higher = closer,
// matching the BM25 direction used elsewhere in the search
// subsystem.
type Hit struct {
	AssetID    uuid.UUID
	Similarity float64
}

// Fetcher pulls a stored embedding for a given asset. Wraps the
// asset_embedding_d768 read path so callers don't need to depend
// on the ai/embeddings sqlc-generated queries directly.
type Fetcher struct {
	Pool     *pgxpool.Pool
	Provider string
	Model    string
	Modality string
}

// NewFetcher constructs a Fetcher with empty (provider, model,
// modality) filters — the anchor row's tuple wins, and Query
// candidates are filtered by that same tuple. This is the
// provider-agnostic default; callers who need to pin a specific
// provider can set the fields after construction.
func NewFetcher(pool *pgxpool.Pool) *Fetcher {
	return &Fetcher{Pool: pool}
}

// Anchor is a fetched embedding with the (provider, model,
// modality) tuple it was stored under. The tuple is preserved so
// the kNN query can compare against candidates written under the
// same tuple — different providers/models embed into different
// vector spaces and similarity between them is meaningless.
type Anchor struct {
	Raw      string
	Provider string
	Model    string
	Modality string
}

// FetchAssetEmbedding returns the pgvector-formatted anchor for
// the asset's stored embedding. If the Fetcher has explicit
// Provider/Model/Modality values set, they gate the lookup;
// otherwise the first row for the asset wins (typical case: each
// install runs one provider). Returns [ErrNotEmbedded] if no row
// exists.
//
// Callers pass the returned Anchor.Raw to [Query] via a pgvector
// `$N::vector` cast; the (provider, model, modality) triple flows
// to Query so it filters candidates by the same tuple.
func (f *Fetcher) FetchAssetEmbedding(ctx context.Context, assetID uuid.UUID) (Anchor, error) {
	sql := `
		SELECT embedding::text, provider, model, modality
		  FROM asset_embedding_d768
		 WHERE asset_id = $1
	`
	args := []any{assetID}
	if f.Provider != "" {
		sql += ` AND provider = $2 AND model = $3 AND modality = $4`
		args = append(args, f.Provider, f.Model, f.Modality)
	}
	sql += ` LIMIT 1`
	var out Anchor
	err := f.Pool.QueryRow(ctx, sql, args...).Scan(&out.Raw, &out.Provider, &out.Model, &out.Modality)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Anchor{}, ErrNotEmbedded
		}
		return Anchor{}, fmt.Errorf("vector: fetch embedding: %w", err)
	}
	return out, nil
}

// EncodeFloat32Slice renders a []float32 into the pgvector
// literal-cast form '[a,b,c,...]' — the ONLY reliable way to
// send a float32 slice to pgvector via pgx without importing the
// pgvector-go binding. Used by the reserved by-image path if/when
// a real image encoder ships; today's text-hybrid path fetches
// stored embeddings verbatim via FetchAssetEmbedding and skips
// this encoder.
func EncodeFloat32Slice(vec []float32) string {
	if len(vec) == 0 {
		return "[]"
	}
	var sb strings.Builder
	sb.Grow(len(vec) * 12)
	sb.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(v), 'g', 6, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

// Query runs a kNN similarity query against asset_embedding_d768,
// visibility-gated per Phase 1.16.B-2's shared helper. Returns up
// to `limit` hits sorted by descending similarity; hits scoring
// below `threshold` are dropped.
//
// The anchor's (Provider, Model, Modality) tuple filters candidates
// so all pairwise comparisons happen in the same vector space —
// different models embed into incompatible spaces and cross-model
// similarity is meaningless.
//
// Cursor pagination is NOT implemented here — the search Engine's
// hybrid path merges vector hits with BM25 hits in memory and
// emits a unified cursor. This function is the raw kNN primitive.
func Query(ctx context.Context, pool *pgxpool.Pool, anchor Anchor, caller visibility.Caller, threshold float64, limit int) ([]Hit, error) {
	if anchor.Raw == "" {
		return nil, ErrEmptyAnchor
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 200 {
		limit = 200
	}
	if threshold < 0 {
		threshold = 0
	}
	if threshold > 1 {
		threshold = 1
	}

	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, err
	}
	visFrag, visArgs := pred.ToSQL("a", 6)

	sql := `
		SELECT ae.asset_id,
		       1 - (ae.embedding <=> $1::vector) AS sim
		  FROM asset_embedding_d768 ae
		  JOIN assets a ON a.id = ae.asset_id
		 WHERE ae.provider = $2
		   AND ae.model    = $3
		   AND ae.modality = $4
		   AND 1 - (ae.embedding <=> $1::vector) >= $5` + visFrag + `
		 ORDER BY ae.embedding <=> $1::vector ASC
		 LIMIT $6
	`
	args := []any{anchor.Raw, anchor.Provider, anchor.Model, anchor.Modality, threshold, limit}
	args = append(args, visArgs...)

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("vector: kNN query: %w", err)
	}
	defer rows.Close()

	out := make([]Hit, 0, limit)
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.AssetID, &h.Similarity); err != nil {
			return nil, fmt.Errorf("vector: scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Sentinels — HTTP layer maps to appropriate status codes.
var (
	// ErrNotEmbedded is returned by FetchAssetEmbedding when the
	// asset exists but has no row in asset_embedding_d768 for the
	// (provider, model, modality) tuple. DSL compiler maps to
	// HTTP 404 with a clear "asset not yet embedded" message.
	ErrNotEmbedded = errors.New("vector: asset has no embedding")

	// ErrEmptyAnchor is returned by Query when the caller supplies
	// an empty anchor literal. Indicates a programmer bug rather
	// than a runtime condition.
	ErrEmptyAnchor = errors.New("vector: empty anchor embedding")
)
