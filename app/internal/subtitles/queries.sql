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
--
-- asset_file_hash (#1596): when non-empty, the track is written only
-- while the asset still shows those original bytes. A transcription
-- of a replaced original matches nothing (no row returned) instead of
-- attaching the old recording's words to the new one. The row lock is
-- taken in the same statement as the write, so a replacement either
-- commits first (no match) or waits for this write and supersedes it.
-- NO KEY UPDATE, not SHARE, so two writers on one asset queue rather
-- than deadlock. Empty means "no expectation" (a person's upload).
WITH cur AS (
    SELECT id FROM assets
     WHERE id = sqlc.arg('asset_id')
       AND (sqlc.arg('asset_file_hash')::TEXT = ''
            OR file_hash = sqlc.arg('asset_file_hash')::TEXT)
       FOR NO KEY UPDATE
)
INSERT INTO asset_subtitle_tracks
    (asset_id, lang, label, file_hash, source_format, confidence)
SELECT cur.id, sqlc.arg('lang'), sqlc.arg('label'), sqlc.arg('file_hash'),
       sqlc.arg('source_format'), sqlc.arg('confidence')
  FROM cur
ON CONFLICT (asset_id, lang) DO UPDATE
    SET label = EXCLUDED.label,
        file_hash = EXCLUDED.file_hash,
        source_format = EXCLUDED.source_format,
        confidence = EXCLUDED.confidence,
        created_at = NOW()
RETURNING asset_id, lang, label, file_hash, source_format, confidence, created_at;

-- name: DeleteSubtitleTrack :one
-- RETURNING the removed track's bytes: the handler releases their pin
-- in the same transaction (#1596), and no row (pgx.ErrNoRows) is the
-- 404. Cache invalidation MUST follow at the handler layer.
DELETE FROM asset_subtitle_tracks
 WHERE asset_id = $1 AND lang = $2
RETURNING file_hash;

-- name: LockAssetForTrackWrite :one
-- #1596: every track row change locks the asset row first (the order the
-- replacement uses: asset, then track rows, then object rows), and, with
-- asset_file_hash, only while the asset still shows those original bytes.
-- No row means the asset is gone or, with a hash, replaced.
SELECT id FROM assets
 WHERE id = sqlc.arg('asset_id')
   AND (sqlc.arg('asset_file_hash')::TEXT = ''
        OR file_hash = sqlc.arg('asset_file_hash')::TEXT)
   FOR NO KEY UPDATE;

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

-- name: RetireAITracksForAsset :many
-- #1596: an original replacement removes the AI tracks made from the
-- recording it replaces, in the replacement's transaction. Only
-- source_format 'whisper' is AI-produced: the upload API accepts
-- vtt/srt/ssa/ass/sub/idx and never writes it, so a person's track is
-- never matched. RETURNING gives the caller each removed track's bytes,
-- whose pin it then releases.
DELETE FROM asset_subtitle_tracks
 WHERE asset_id = $1
   AND source_format = 'whisper'
RETURNING lang, file_hash;

-- name: ReleaseUnusedTrackPin :execrows
-- #1596: drops one (asset, lang) track pin from bytes no live track of
-- that (asset, lang) uses. A track's pin is keyed (VTT hash,
-- 'subtitle_track', '<asset>-<lang>'), and identical VTT bytes share
-- that one row, so the NOT EXISTS is what keeps a retired or refused
-- track's release from unpinning a live track with the same bytes. The
-- caller locks the object row first (LockObjectForPinChange), so this
-- statement's snapshot sees any track row committed before it; a track
-- written after it re-asserts its own pin in its own transaction (see
-- Handler.Upsert).
DELETE FROM storage_pins
 WHERE object_hash = sqlc.arg('object_hash')
   AND pin_subject_type = 'subtitle_track'
   AND pin_subject_id = sqlc.arg('pin_subject_id')
   AND NOT EXISTS (
       SELECT 1 FROM asset_subtitle_tracks t
        WHERE t.asset_id = sqlc.arg('asset_id')
          AND t.lang = sqlc.arg('lang')
          AND t.file_hash = sqlc.arg('object_hash'));

-- name: EnsureTrackPin :exec
-- #1596: a track row's pin, asserted in the row's own transaction (see
-- Handler.Upsert). Only for bytes that exist: the object row is the
-- caller's lock target, and a hash with no object has nothing to hold.
INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id)
SELECT hash, 'subtitle_track', sqlc.arg('pin_subject_id')
  FROM storage_objects
 WHERE hash = sqlc.arg('object_hash')
ON CONFLICT (object_hash, pin_subject_type, pin_subject_id) DO NOTHING;
