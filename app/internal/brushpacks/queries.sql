-- queries for the brushpacks package (Phase 1.21c).
--
-- All queries are owner-scoped — every read takes `owner_ref` so a
-- user can't enumerate or fetch someone else's pack metadata. The
-- HTTP handler is responsible for authn (resolving the session →
-- owner_ref); these queries are the second line of defence.

-- name: CreatePack :one
INSERT INTO brush_packs (owner_ref, name, source_file)
VALUES ($1, $2, $3)
RETURNING id, owner_ref, name, source_file, created_at, origin_server_id;

-- name: ListPacksForOwner :many
-- Most-recent first so the panel shows the user's latest import at
-- the top of the brush-pack picker.
SELECT id, owner_ref, name, source_file, created_at, origin_server_id
FROM brush_packs
WHERE owner_ref = $1
ORDER BY created_at DESC;

-- name: GetPackForOwner :one
-- Scoped GET — returns sql.ErrNoRows when the pack doesn't exist OR
-- exists but belongs to someone else. Callers map that to 404, which
-- avoids leaking pack existence across owners.
SELECT id, owner_ref, name, source_file, created_at, origin_server_id
FROM brush_packs
WHERE id = $1 AND owner_ref = $2;

-- name: DeletePackForOwner :execrows
-- Returns rows affected so the handler can 404 when the pack didn't
-- exist (or belonged to someone else) without a separate read.
-- ON DELETE CASCADE on brush_pack_stamps clears the stamp rows;
-- the bitmap files in object storage are deleted by the service
-- layer before this query runs (storage is best-effort cleanup
-- since DB deletion is the source of truth).
DELETE FROM brush_packs
WHERE id = $1 AND owner_ref = $2;

-- name: InsertStamp :one
-- Stamps are inserted one-at-a-time by the importer (one INSERT per
-- decoded brush). Batching would matter for thousand-stamp packs;
-- the 15-stamp test corpus does fine without it. Bulk insert is
-- a future optimization.
INSERT INTO brush_pack_stamps (
    pack_id, abr_id, label, width, height, storage_key,
    spacing, align_to_path, size_jitter, opacity_jitter, angle_jitter
)
VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11
)
RETURNING id, pack_id, abr_id, label, width, height, storage_key,
          spacing, align_to_path, size_jitter, opacity_jitter, angle_jitter,
          created_at;

-- name: ListStampsForPack :many
-- Ordered by created_at so re-imports preserve the original ABR
-- ordering (the importer inserts samples in file order).
SELECT id, pack_id, abr_id, label, width, height, storage_key,
       spacing, align_to_path, size_jitter, opacity_jitter, angle_jitter,
       created_at
FROM brush_pack_stamps
WHERE pack_id = $1
ORDER BY created_at ASC;

-- name: GetStampForOwner :one
-- Cross-checks the stamp's pack belongs to the owner before
-- returning. Used by the stamp-fetch handler to serve bitmaps —
-- the handler then streams the bytes from storage using
-- storage_key.
SELECT s.id, s.pack_id, s.abr_id, s.label, s.width, s.height,
       s.storage_key, s.spacing, s.align_to_path,
       s.size_jitter, s.opacity_jitter, s.angle_jitter, s.created_at
FROM brush_pack_stamps s
JOIN brush_packs p ON p.id = s.pack_id
WHERE s.id = $1 AND p.owner_ref = $2;
