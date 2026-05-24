-- name: List :many
-- List every resource type, sorted by display order then by ID. Used
-- by GET /api/v1/resource_types and by anything that needs the lookup
-- table in memory.
SELECT ref,
       name,
       allowed_extensions,
       order_by,
       icon,
       colour,
       tab
FROM resource_type
ORDER BY COALESCE(order_by, 0), ref;

-- name: Get :one
-- Fetch a single resource type by primary key. Returns sql.ErrNoRows
-- when missing.
SELECT ref,
       name,
       allowed_extensions,
       order_by,
       icon,
       colour,
       tab
FROM resource_type
WHERE ref = $1;
