-- app/internal/subtitles/queries.sql
--
-- Phase 1.18.B-3 — subtitle / caption track storage.
-- Schema lives in 00002_subtitle_tracks.sql (the first
-- post-baseline migration per ADR 0046).

-- name: ListSubtitleTracksForAsset :many
-- Read path. Cache-fronted via cacheDomainAssetSubtitleTracks.
-- Order by lang ASC for deterministic UI rendering.
SELECT asset_id, lang, label, file_hash, source_format, confidence, created_at
  FROM asset_subtitle_tracks
 WHERE asset_id = $1
 ORDER BY lang ASC;

-- name: GetSubtitleTrack :one
-- Single-track lookup. Used by the delete endpoint to surface
-- a 404 when the (asset, lang) tuple doesn't exist + by the
-- conversion job to confirm post-write state.
SELECT asset_id, lang, label, file_hash, source_format, confidence, created_at
  FROM asset_subtitle_tracks
 WHERE asset_id = $1 AND lang = $2;

-- name: UpsertSubtitleTrack :one
-- Idempotent insert. Re-upload of the same (asset, lang) overwrites
-- the file_hash + source_format + confidence (operator action via
-- the upload endpoint OR conversion-job retry). Cache invalidation
-- MUST follow this query at the handler layer.
INSERT INTO asset_subtitle_tracks
    (asset_id, lang, label, file_hash, source_format, confidence)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (asset_id, lang) DO UPDATE
    SET label = EXCLUDED.label,
        file_hash = EXCLUDED.file_hash,
        source_format = EXCLUDED.source_format,
        confidence = EXCLUDED.confidence,
        created_at = NOW()
RETURNING asset_id, lang, label, file_hash, source_format, confidence, created_at;

-- name: DeleteSubtitleTrack :execrows
-- Returns the number of rows affected so the handler can
-- distinguish 204 (deleted) from 404 (not-found) without a
-- prior GET round-trip. Cache invalidation MUST follow at the
-- handler layer.
DELETE FROM asset_subtitle_tracks
 WHERE asset_id = $1 AND lang = $2;

-- name: CountSubtitleTracksForAsset :one
-- Observability + admin surface support. NOT used in any quota
-- check — subtitles don't count.
SELECT count(*) FROM asset_subtitle_tracks WHERE asset_id = $1;

-- name: GetAssetRenderableKind :one
-- Phase 1.18.B-3 policy gate. Returns the asset_types.name (Image,
-- Video, Audio, etc.) so the RequiresAudioVideo guard can decide
-- whether subtitle endpoints apply. Standalone in this package
-- rather than via assets.Handler to keep the dep graph one-way
-- (subtitles does NOT pull on assets, which would create cycles
-- when assets imports subtitles for cache invalidation).
SELECT at.name
  FROM assets a
  JOIN asset_types at ON at.ref = a.asset_type
 WHERE a.id = $1
   AND a.deleted_at IS NULL;
