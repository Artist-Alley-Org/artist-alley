package metadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// EmbeddedPreviewVariantKey is the storage variant key used for the
// JPEG preview extracted from a raw camera file. The variant
// pipeline checks for this key on raw-extension uploads and uses
// the bytes as the source instead of trying to demosaic the raw.
const EmbeddedPreviewVariantKey = "embedded-preview"

// EmbeddedPreviewContentType is the MIME type recorded with the
// embedded-preview variant row. Always JPEG today — every raw
// camera firmware writes JPEG previews regardless of the body's
// native raw format.
const EmbeddedPreviewContentType = "image/jpeg"

// PoolAssetAttributeWriter is the production [AssetAttributeWriter]
// backed by a pgxpool. Wrapped around the existing assets queries —
// no new SQL beyond the migration's SetAssetPageCount.
type PoolAssetAttributeWriter struct {
	pool *pgxpool.Pool
}

// NewPoolAssetAttributeWriter wires the adapter against the shared
// pool. Boot calls this once + passes the result into the
// ExtractJobHandler via WithAssetAttributes.
func NewPoolAssetAttributeWriter(pool *pgxpool.Pool) *PoolAssetAttributeWriter {
	return &PoolAssetAttributeWriter{pool: pool}
}

// SetAssetPageCount implements [AssetAttributeWriter]. Zero
// pageCount is a no-op so extractors can call unconditionally.
func (w *PoolAssetAttributeWriter) SetAssetPageCount(ctx context.Context, assetID uuid.UUID, pageCount int) error {
	if w == nil || w.pool == nil {
		return errors.New("metadata: PoolAssetAttributeWriter: nil pool")
	}
	if pageCount <= 0 {
		return nil
	}
	n := int32(pageCount)
	return assets.New(w.pool).SetAssetPageCount(ctx, assets.SetAssetPageCountParams{
		ID:        pgtype.UUID{Bytes: assetID, Valid: true},
		PageCount: &n,
	})
}

// StoragePreviewVariantWriter is the production
// [PreviewVariantWriter] backed by the storage service +
// storage_variants table. Idempotent: if the embedded-preview
// variant already exists for the given hash, the call is a no-op so
// re-extraction doesn't churn the backend.
type StoragePreviewVariantWriter struct {
	pool    *pgxpool.Pool
	storage *storage.Service
}

// NewStoragePreviewVariantWriter wires the adapter against the
// shared storage service.
func NewStoragePreviewVariantWriter(pool *pgxpool.Pool, st *storage.Service) *StoragePreviewVariantWriter {
	return &StoragePreviewVariantWriter{pool: pool, storage: st}
}

// WriteEmbeddedPreview implements [PreviewVariantWriter].
func (w *StoragePreviewVariantWriter) WriteEmbeddedPreview(ctx context.Context, fileHash string, jpeg []byte) error {
	if w == nil || w.storage == nil || w.pool == nil {
		return errors.New("metadata: StoragePreviewVariantWriter: nil storage / pool")
	}
	if fileHash == "" || len(jpeg) == 0 {
		return nil
	}
	if err := storage.ValidateHash(fileHash); err != nil {
		return fmt.Errorf("metadata: embedded-preview: %w", err)
	}

	// Idempotency: skip the upload if the variant already exists.
	// Storage.Stat returns ErrNotFound when missing; any other
	// error we let bubble so the worker retries.
	if _, err := w.storage.Backend.Stat(ctx, fileHash, EmbeddedPreviewVariantKey); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("metadata: embedded-preview stat: %w", err)
	}

	if _, err := w.storage.Backend.Put(ctx, fileHash, EmbeddedPreviewVariantKey, bytes.NewReader(jpeg)); err != nil {
		return fmt.Errorf("metadata: embedded-preview put: %w", err)
	}
	if err := storage.New(w.pool).UpsertVariant(ctx, storage.UpsertVariantParams{
		ObjectHash:  fileHash,
		VariantKey:  EmbeddedPreviewVariantKey,
		SizeBytes:   int64(len(jpeg)),
		ContentType: EmbeddedPreviewContentType,
		Metadata:    []byte("{}"),
	}); err != nil {
		return fmt.Errorf("metadata: embedded-preview upsert: %w", err)
	}
	return nil
}

// Compile-time conformance.
var (
	_ AssetAttributeWriter = (*PoolAssetAttributeWriter)(nil)
	_ PreviewVariantWriter = (*StoragePreviewVariantWriter)(nil)
)
