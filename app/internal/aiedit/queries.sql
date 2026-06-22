-- Phase 1.14.E-1 — aiedit subsystem queries.
--
-- Append-only by intent (per ADR 0046 once v1.0.0 ships; pre-v1 the
-- schema can still evolve). All queries here are write-once or pure
-- reads — the table is the source-of-truth for "this asset was
-- generated from that asset", never mutated after insert.

-- name: InsertCreativeLineage :one
-- Records a derivative→source link with provider-specific metadata.
-- Returns the row so the caller can include created_at in its
-- response without a round-trip.
INSERT INTO creative_lineage (
    derivative_asset_id,
    source_asset_id,
    generation_metadata
) VALUES (
    $1, $2, $3
)
RETURNING derivative_asset_id, source_asset_id, generation_metadata, created_at;

-- name: GetCreativeLineageByDerivative :one
-- Reverse lookup: given a derivative asset id, what's its source +
-- the generation metadata that produced it. Powers the "this asset
-- was generated from X with prompt Y" detail panel in E-2.
SELECT derivative_asset_id, source_asset_id, generation_metadata, created_at
  FROM creative_lineage
 WHERE derivative_asset_id = $1;

-- name: ListCreativeLineageBySource :many
-- Forward lookup: every derivative spawned from this source. Powers
-- the "see all variations of this asset" surface in E-2 + future
-- moderation flows ("show me every AI-generated child of asset X").
-- Ordered newest-first so the most recent variation surfaces at the
-- top of the gallery.
SELECT derivative_asset_id, source_asset_id, generation_metadata, created_at
  FROM creative_lineage
 WHERE source_asset_id = $1
 ORDER BY created_at DESC;
