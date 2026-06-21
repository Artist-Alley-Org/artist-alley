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

-- name: FindSimilarAssetsByAnchorD768 :many
-- One-shot anchor-then-kNN. Joins the anchor's vector via a CTE +
-- ranks every other (provider, model, modality)-matching row by
-- cosine distance. Lower distance = more similar.
--
-- The subquery / CTE form is what works around sqlc's interface{}
-- type for the vector column — passing the anchor vector as a
-- parameter would force a *pgvector.Vector binding on the caller,
-- which means an extra round-trip to fetch it first. One query
-- here, sqlc-friendly typed params, no Go-side vector handling.
WITH anchor AS (
    SELECT embedding
    FROM asset_embedding_d768 a
    WHERE a.asset_id = sqlc.arg('anchor_asset_id')
      AND a.provider = $1
      AND a.model    = $2
      AND a.modality = $3
)
SELECT
    e.asset_id,
    (e.embedding <=> (SELECT embedding FROM anchor)) AS distance
FROM asset_embedding_d768 e
WHERE e.provider = $1
  AND e.model    = $2
  AND e.modality = $3
  AND e.asset_id <> sqlc.arg('anchor_asset_id')
  AND EXISTS (SELECT 1 FROM anchor)
ORDER BY e.embedding <=> (SELECT embedding FROM anchor) ASC
LIMIT sqlc.arg('result_limit')::INTEGER;

-- name: AssetEmbeddingExistsD768 :one
-- True when the anchor has an embedding row for the requested
-- (provider, model, modality). Used by the search handler to
-- distinguish "no neighbours" from "embedding pending" in the
-- response.
SELECT EXISTS (
    SELECT 1 FROM asset_embedding_d768
    WHERE asset_id = $1 AND provider = $2 AND model = $3 AND modality = $4
)::BOOLEAN AS exists;

-- name: DeleteAssetEmbeddingsForAssetD768 :exec
-- Cleanup helper used by the asset soft-delete path so a re-uploaded
-- asset gets a fresh embedding instead of inheriting the stale one.
-- The ON DELETE CASCADE on the FK handles hard-delete cleanup.
DELETE FROM asset_embedding_d768 WHERE asset_id = $1;
