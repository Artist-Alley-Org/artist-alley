// Phase 1.14.B — concrete ai.EmbeddingWriter implementation.
//
// Replaces ai.NewStubEmbeddingWriter at boot. Dispatches the upsert
// to the per-dim sibling table that matches the input model's dim
// (see dim_registry.go for the rationale on per-dim tables).
//
// # Idempotency
//
// The composite PK (asset_id, provider, model, modality) drives the
// upsert. Re-running embedding for the same key with the same
// content_hash is a no-op write to the operator (same vector
// re-stored). Same key with a different content_hash means the
// asset bytes changed since the last embedding — the writer
// happily overwrites; the audit row's updated_at column records
// the latest write time.

package embeddings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

// ErrUnsupportedModel is returned when the input names a model that
// isn't in `ai.embedding.dim_registry`. Job handlers map this to
// terminal (operator config issue; no retry).
var ErrUnsupportedModel = errors.New("embeddings: model not registered in ai.embedding.dim_registry")

// ErrDimensionMismatch is returned when the input vector's length
// doesn't match the registered dim for the model. Provider bug;
// terminal.
var ErrDimensionMismatch = errors.New("embeddings: vector length doesn't match registered dim for model")

// Writer is the production implementation of ai.EmbeddingWriter.
type Writer struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	dims   *DimRegistry
}

// NewWriter constructs the writer + warms the dim registry. Returns
// an error if the registry can't be loaded — fail-fast at boot
// rather than first-write-time surprise.
func NewWriter(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (*Writer, error) {
	dims := NewDimRegistry(pool)
	if _, err := dims.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("embeddings.NewWriter: warm dim registry: %w", err)
	}
	return &Writer{pool: pool, logger: logger, dims: dims}, nil
}

// DimRegistry exposes the live registry so the admin surface can
// surface "which models are registered" + trigger refresh.
func (w *Writer) DimRegistry() *DimRegistry { return w.dims }

// UpsertAssetEmbedding implements ai.EmbeddingWriter.
//
// Validates the input dim against the registered dim for the model,
// then dispatches to the per-dim sibling table. The bridge sentinel
// ai.ErrAssetNotFound surfaces when the FK violates on insert (asset
// got deleted between job enqueue + execution).
func (w *Writer) UpsertAssetEmbedding(ctx context.Context, in ai.EmbeddingInput) error {
	if in.AssetID == uuid.Nil {
		return fmt.Errorf("embeddings.Upsert: asset_id required")
	}
	if in.Model == "" {
		return fmt.Errorf("embeddings.Upsert: model required")
	}
	if in.Provider == "" {
		return fmt.Errorf("embeddings.Upsert: provider required")
	}
	if in.Modality == "" {
		return fmt.Errorf("embeddings.Upsert: modality required")
	}

	dim, ok := w.dims.DimForModel(in.Model)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnsupportedModel, in.Model)
	}
	if len(in.Vector) != dim {
		return fmt.Errorf("%w: model %q expects dim=%d, got %d",
			ErrDimensionMismatch, in.Model, dim, len(in.Vector))
	}

	var hashPtr *string
	if in.ContentHash != "" {
		h := in.ContentHash
		hashPtr = &h
	}

	q := New(w.pool)
	vec := pgvector.NewVector(in.Vector)

	switch dim {
	case 768:
		err := q.UpsertAssetEmbeddingD768(ctx, UpsertAssetEmbeddingD768Params{
			AssetID:     pgtype.UUID{Bytes: in.AssetID, Valid: true},
			Provider:    in.Provider,
			Model:       in.Model,
			Modality:    in.Modality,
			Column5:     &vec,
			ContentHash: hashPtr,
		})
		if err != nil {
			return classifyUpsertError(err, in.AssetID)
		}
	default:
		// SupportedDims validation in DimRegistry should make this
		// unreachable, but defensive — a future operator who adds a
		// new dim to dim_registry without updating SupportedDims
		// gets a clean error rather than a panic.
		return fmt.Errorf("embeddings: dim %d registered for model %q but no upsert case wired", dim, in.Model)
	}

	w.logger.Info("ai.embedding.upsert",
		"asset_id", in.AssetID.String(),
		"provider", in.Provider,
		"model", in.Model,
		"modality", in.Modality,
		"dim", dim)
	return nil
}

// classifyUpsertError maps a pgx error to the right caller-visible
// sentinel. FK violation → ai.ErrAssetNotFound; everything else
// passes through wrapped.
func classifyUpsertError(err error, assetID uuid.UUID) error {
	var pgErr *pgconnError
	if errors.As(err, &pgErr) && pgErr.Code == fkViolationCode {
		return ai.ErrAssetNotFound
	}
	// The pgx error type is unexported until pgconn surfaces it;
	// string-matching on the message is the back-stop. Cheaper than
	// importing pgconn here just for one classifier.
	if errStringContains(err, "asset_embedding_d") && errStringContains(err, "foreign key") {
		return fmt.Errorf("%w: asset %s", ai.ErrAssetNotFound, assetID)
	}
	return fmt.Errorf("embeddings.Upsert: %w", err)
}

// pgconnError + fkViolationCode are placeholders so the typed
// classifier reads cleanly above. We rely on the string fallback in
// errStringContains — pgx wraps errors but the SQLSTATE shows up in
// the message ("ERROR: ... (SQLSTATE 23503)").
type pgconnError struct{ Code string }

func (e *pgconnError) Error() string { return e.Code }

const fkViolationCode = "23503"

func errStringContains(err error, sub string) bool {
	return err != nil && contains(err.Error(), sub)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Compile-time interface satisfaction.
var _ ai.EmbeddingWriter = (*Writer)(nil)

// Unused import guard — pgx import retained for the FK-violation
// classifier's eventual typed migration.
var _ = pgx.ErrNoRows
