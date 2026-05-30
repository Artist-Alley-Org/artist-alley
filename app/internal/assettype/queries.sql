-- name: List :many
-- List every asset type, sorted by display order then by ID. Used
-- by GET /api/v1/asset_types and by anything that needs the lookup
-- table in memory.
SELECT ref,
       name,
       allowed_extensions,
       order_by,
       icon,
       colour,
       tab
FROM asset_types
ORDER BY COALESCE(order_by, 0), ref;

-- name: Get :one
-- Fetch a single asset type by primary key. Returns sql.ErrNoRows
-- when missing.
SELECT ref,
       name,
       allowed_extensions,
       order_by,
       icon,
       colour,
       tab
FROM asset_types
WHERE ref = $1;
