-- app/internal/ai/embeddings/queries.sql
--
-- Phase 1.14.B — per-dim asset_embedding sibling tables.
--
-- pgvector + Postgres declarative partitioning is incompatible with
-- per-dim variation (partitions must share column types). So we
-- ship one sibling table per vector dim — `asset_embedding_d768`
-- here, `asset_embedding_d1024` etc. when added by follow-up
-- migrations. The Go-side writer in writer.go dispatches by
-- model → dim via system_config's `ai.embedding.dim_registry`.
--
-- Two reasons to NOT abstract these via a string-substituted table
-- (Go-side): sqlc is a compile-time tool and can't take a runtime
-- table identifier, and prepared-statement caching wants stable
-- SQL text.

-- name: UpsertAssetEmbeddingD768 :exec
-- Idempotent: re-running embedding for the same (asset, provider,
-- model, modality) key replaces the vector. Sets updated_at on
-- conflict so the audit trail records the latest write time.
INSERT INTO asset_embedding_d768
    (asset_id, provider, model, modality, embedding, content_hash, updated_at)
VALUES ($1, $2, $3, $4, $5::vector, sqlc.narg('content_hash')::TEXT, NOW())
ON CONFLICT (asset_id, provider, model, modality) DO UPDATE
    SET embedding    = EXCLUDED.embedding,
        content_hash = EXCLUDED.content_hash,
        updated_at   = NOW();

-- name: GetAssetEmbeddingD768 :one
-- Read one embedding by full key. Used by the similarity-search
-- handler to grab the query vector before running kNN. modality is
-- cast to TEXT in the projection so the Go side handles a plain
-- string instead of the ai_embedding_modality domain type.
SELECT
    asset_id,
    provider,
    model,
    modality::TEXT AS modality_text,
    embedding,
    content_hash,
    created_at,
    updated_at
FROM asset_embedding_d768
WHERE asset_id = $1 AND provider = $2 AND model = $3 AND modality = $4;

-- name: FindSimilarAssetsD768 :many
-- Nearest-neighbour search over the HNSW cosine index. $1 is the
-- query vector; $2/$3/$4 are the search space (different models
-- live in different vector spaces — cross-model cosine is
-- meaningless). exclude_asset_id excludes the anchor;
-- result_limit caps the response.
--
-- Distance is cosine — pgvector's `<=>` operator. Lower = more
-- similar; 0.0 = identical vectors.
SELECT
    e.asset_id,
    (e.embedding <=> $1::vector) AS distance
FROM asset_embedding_d768 e
WHERE e.provider = $2
  AND e.model    = $3
  AND e.modality = $4
  AND e.asset_id <> sqlc.arg('exclude_asset_id')
ORDER BY e.embedding <=> $1::vector ASC
LIMIT sqlc.arg('result_limit')::INTEGER;

-- name: DeleteAssetEmbeddingsForAssetD768 :exec
-- Cleanup helper used by the asset soft-delete path so a re-uploaded
-- asset gets a fresh embedding instead of inheriting the stale one.
-- The ON DELETE CASCADE on the FK handles hard-delete cleanup.
DELETE FROM asset_embedding_d768 WHERE asset_id = $1;
