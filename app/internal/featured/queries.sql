-- GitHub #341 — admin-curated featured_items queries.
--
-- Naming convention: InsertX / ListX / DeleteX / UpdateX mirrors the
-- existing requests + users packages.

-- name: ListFeaturedItems :many
-- Ordered curation list. LEFT JOINs both subject tables so a single
-- read resolves the display title (asset.title or collection.name)
-- plus the asset thumbnail hints, without the handler fanning out a
-- per-row lookup. A dangling reference (subject hard-deleted) yields
-- an empty title rather than dropping the row — the operator prunes
-- stale entries by hand.
SELECT f.id, f.subject_kind, f.subject_id, f.position,
       f.created_at, f.created_by_user_ref,
       COALESCE(a.title, c.name, '')::text AS title,
       a.file_hash AS asset_file_hash,
       COALESCE(a.has_image, false)::boolean AS asset_has_image
FROM featured_items f
LEFT JOIN assets a
       ON f.subject_kind = 'asset' AND a.id = f.subject_id
LEFT JOIN collections c
       ON f.subject_kind = 'collection' AND c.id = f.subject_id
ORDER BY f.position ASC, f.created_at ASC;

-- name: InsertFeaturedItem :one
-- Adds one subject to the curation list. When position is NULL the
-- row appends to the end (max existing position + 1). The
-- (subject_kind, subject_id) unique constraint makes a duplicate add
-- a 23505 the handler maps to 409.
INSERT INTO featured_items (subject_kind, subject_id, position, created_by_user_ref)
VALUES (
    $1,
    $2,
    COALESCE(sqlc.narg('position')::integer,
             (SELECT COALESCE(MAX(position), -1) + 1 FROM featured_items)),
    sqlc.narg('created_by_user_ref')::bigint
)
RETURNING id, subject_kind, subject_id, position, created_at, created_by_user_ref;

-- name: DeleteFeaturedItem :execrows
-- Removes one entry by id. Returns rows-affected so the handler can
-- map 0 → 404.
DELETE FROM featured_items WHERE id = $1;

-- name: UpdateFeaturedPosition :execrows
-- Sets the ordering position of one entry. Called once per row inside
-- the reorder transaction. Returns rows-affected for the same 0 → 404
-- mapping.
UPDATE featured_items SET position = $2 WHERE id = $1;
