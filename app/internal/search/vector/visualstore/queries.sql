-- Phase 1.16.B-3-followup — asset_visual_embedding row I/O + query.

-- name: UpsertAssetVisualEmbedding :exec
-- Idempotent: re-embedding an asset (backfill re-run OR sidecar
-- model swap) updates the row + touches updated_at. Provider +
-- model + checkpoint are recorded per row so a future "reindex
-- visual embeddings against a different model" pass can target
-- rows by (provider, model, checkpoint).
INSERT INTO asset_visual_embedding
    (asset_id, embedding, model, checkpoint, provider, updated_at)
VALUES ($1, $2::vector, $3, $4, $5, NOW())
ON CONFLICT (asset_id) DO UPDATE
    SET embedding  = EXCLUDED.embedding,
        model      = EXCLUDED.model,
        checkpoint = EXCLUDED.checkpoint,
        provider   = EXCLUDED.provider,
        updated_at = NOW();

-- name: DeleteAssetVisualEmbedding :exec
DELETE FROM asset_visual_embedding WHERE asset_id = $1;

-- name: SearchByVisualEmbedding :many
-- Cosine-similarity top-K over asset_visual_embedding. Uses the
-- HNSW index (idx_asset_visual_embedding_hnsw_cosine). Returns
-- asset_id + similarity so the by-image handler can layer
-- visibility.Filter downstream + look up asset detail projections
-- for the response.
--
-- (1 - (embedding <=> query))  → cosine similarity in [0, 2]
-- (higher = more similar). Convert to [0, 1] on the caller side
-- for consistency with the text-search score range.
SELECT asset_id,
       (1 - (embedding <=> $1::vector))::REAL AS similarity
FROM asset_visual_embedding
ORDER BY embedding <=> $1::vector
LIMIT $2;

-- name: CountAssetVisualEmbeddings :one
-- Health gauge: total visual embeddings written. Surfaces on
-- /admin/search/health.
SELECT COUNT(*)::BIGINT AS total FROM asset_visual_embedding;

-- name: CountVisualEmbeddingBacklog :one
-- Health gauge: image assets that LACK a visual embedding.
-- Anti-join between the image assets CLIP can embed and
-- asset_visual_embedding. Used by the admin dashboard to trigger
-- operator backfill when coverage lags.
--
-- Image-ness is the FILE FORMAT (#579). This used to read
-- `a.has_image = TRUE`, a column with no writer anywhere, so the
-- backlog was 0 for every install and the dashboard reported perfect
-- coverage of nothing. The eligible extensions arrive as a parameter
-- from visualembed.IsImageExtension rather than being inlined here,
-- so this file and the worker's guard cannot disagree about what CLIP
-- can process.
SELECT COUNT(*)::BIGINT AS backlog
FROM assets a
WHERE a.deleted_at IS NULL
  AND LOWER(a.file_extension) = ANY(sqlc.arg('image_extensions')::TEXT[])
  AND NOT EXISTS (
      SELECT 1 FROM asset_visual_embedding v WHERE v.asset_id = a.id
  );

-- name: ListImageAssetsNeedingVisualEmbedding :many
-- Backfill worker's queue: image assets without a visual embedding,
-- oldest-first so the backfill converges predictably. Batched;
-- the caller iterates until zero rows returned.
--
-- Same format predicate as the backlog count above, from the same
-- parameter — a queue that disagreed with its own count would report
-- work it then refused to do.
SELECT a.id, a.file_hash, a.file_extension
FROM assets a
WHERE a.deleted_at IS NULL
  AND LOWER(a.file_extension) = ANY(sqlc.arg('image_extensions')::TEXT[])
  AND a.file_hash IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM asset_visual_embedding v WHERE v.asset_id = a.id
  )
ORDER BY a.created_at ASC
LIMIT $1;
