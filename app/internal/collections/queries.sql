-- ---------------------------------------------------------------------------
-- collections (the entity)
-- ---------------------------------------------------------------------------

-- Column ORDER in the explicit lists below is load-bearing, not style.
-- It matches the physical column order a migrated database actually has
-- (search_text/smart_query were added before the soft-delete columns).
-- When a query's column list matches table order, sqlc reuses the
-- Collection model; when it doesn't, sqlc emits a bespoke per-query Row
-- struct and every caller that assigns to Collection stops compiling.
-- See #420 -- app/schema.sql previously described an order migrations
-- never produce, which is what hid this.

-- name: CreateCollection :one
INSERT INTO collections (
    owner_user_ref, name, description, visibility, membership,
    expires_at, purpose, origin_server_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, owner_user_ref, name, description, visibility, membership,
          expires_at, purpose, origin_server_id,
          created_at, updated_at, search_text, smart_query,
          deleted_at, deleted_reason, deleted_by_user_ref, cover_asset_id,
          featured_cover_asset_id, featured_cover_focal_x, featured_cover_focal_y,
          cover_focal_x, cover_focal_y;

-- name: GetCollection :one
-- Filters soft-deleted rows by default. Admin surfaces reading
-- deleted rows use GetCollectionIncludingDeleted below.
SELECT id, owner_user_ref, name, description, visibility, membership,
       expires_at, purpose, origin_server_id,
       created_at, updated_at, search_text, smart_query,
       deleted_at, deleted_reason, deleted_by_user_ref, cover_asset_id,
       featured_cover_asset_id, featured_cover_focal_x, featured_cover_focal_y,
       cover_focal_x, cover_focal_y
FROM collections
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetCollectionIncludingDeleted :one
-- Same as GetCollection but returns soft-deleted rows too. Used by
-- the Restore path (which needs to Get the row to fire the audit
-- event even after deleted_at IS NOT NULL).
SELECT id, owner_user_ref, name, description, visibility, membership,
       expires_at, purpose, origin_server_id,
       created_at, updated_at, search_text, smart_query,
       deleted_at, deleted_reason, deleted_by_user_ref, cover_asset_id,
       featured_cover_asset_id, featured_cover_focal_x, featured_cover_focal_y,
       cover_focal_x, cover_focal_y
FROM collections
WHERE id = $1;

-- name: UpdateCollection :one
-- Partial update via COALESCE — NULL args keep current values.
--
-- cover_asset_id (#1027) and expires_at (#1073) each need a THIRD state
-- the other columns do not: "remove the value that is there". COALESCE
-- cannot express it, because NULL already means "leave alone" for every
-- column above. The way out is metadata's UpdateFieldDefinition
-- `clear_default`: a companion BOOLEAN and a CASE, in THIS statement
-- rather than a second one, so the write stays inside the single
-- activity-emitting transaction and `updated_at` advances exactly once.
--
-- expires_at wore the COALESCE for three releases and so silently kept
-- the TTL a caller asked to remove; the dedicated ClearCollectionExpiresAt
-- statement that was supposed to cover it was never called by anything
-- and is now gone. Two mechanisms for one job is how the working one
-- ends up being the one nobody wired.
UPDATE collections SET
    name        = COALESCE(sqlc.narg('name'),        name),
    description = COALESCE(sqlc.narg('description'), description),
    visibility  = COALESCE(sqlc.narg('visibility'),  visibility),
    membership  = COALESCE(sqlc.narg('membership'),  membership),
    purpose     = COALESCE(sqlc.narg('purpose'),     purpose),
    expires_at  = CASE WHEN sqlc.arg('clear_expires_at')::BOOLEAN THEN NULL
                       ELSE COALESCE(sqlc.narg('expires_at'), expires_at) END,
    cover_asset_id = CASE WHEN sqlc.arg('clear_cover')::BOOLEAN THEN NULL
                          ELSE COALESCE(sqlc.narg('cover_asset_id'), cover_asset_id) END,
    -- #1207 — the featured rail's own cover, and the focal point for its
    -- 890:500 crop. Three more columns, TWO more clear flags, and the
    -- second one covers a PAIR: a focal point is a point, so "remove the
    -- positioning" is one intention over two columns and giving each
    -- half its own flag would let a caller express half a clear, which
    -- the column CHECK then rejects with a constraint error instead of
    -- the 400 the API should have given.
    --
    -- Note the focal columns COALESCE against themselves as usual, which
    -- is why an explicit 0.5/0.5 has to reach here as a value rather
    -- than as "centre, so send nothing": the handler is what keeps that
    -- distinction, and the CHECK is what stops it being lost silently.
    featured_cover_asset_id = CASE WHEN sqlc.arg('clear_featured_cover')::BOOLEAN THEN NULL
                          ELSE COALESCE(sqlc.narg('featured_cover_asset_id'), featured_cover_asset_id) END,
    featured_cover_focal_x = CASE WHEN sqlc.arg('clear_featured_cover_focal')::BOOLEAN THEN NULL
                          ELSE COALESCE(sqlc.narg('featured_cover_focal_x'), featured_cover_focal_x) END,
    featured_cover_focal_y = CASE WHEN sqlc.arg('clear_featured_cover_focal')::BOOLEAN THEN NULL
                          ELSE COALESCE(sqlc.narg('featured_cover_focal_y'), featured_cover_focal_y) END,
    -- The regular cover's own focal pair, on the SQUARE destination. Its
    -- own clear flag, for the reason the featured pair has one: two
    -- columns, one intention.
    cover_focal_x = CASE WHEN sqlc.arg('clear_cover_focal')::BOOLEAN THEN NULL
                          ELSE COALESCE(sqlc.narg('cover_focal_x'), cover_focal_x) END,
    cover_focal_y = CASE WHEN sqlc.arg('clear_cover_focal')::BOOLEAN THEN NULL
                          ELSE COALESCE(sqlc.narg('cover_focal_y'), cover_focal_y) END,
    updated_at  = NOW()
WHERE id = sqlc.arg('id')
RETURNING id, owner_user_ref, name, description, visibility, membership,
          expires_at, purpose, origin_server_id,
          created_at, updated_at, search_text, smart_query,
          deleted_at, deleted_reason, deleted_by_user_ref, cover_asset_id,
          featured_cover_asset_id, featured_cover_focal_x, featured_cover_focal_y,
          cover_focal_x, cover_focal_y;

-- name: DeleteCollection :exec
-- Phase 1.55.C-1b: soft-delete. Sets deleted_at + deleted_reason on
-- the row rather than hard-DELETE; the nightly softdelete.gc
-- coordinator hard-deletes past sysconfig.CollectionRetentionDays,
-- at which point collection_resources / collection_posts /
-- collection_acls cascade via their existing FK ON DELETE CASCADE.
--
-- deleted_by_user_ref: see the note on assets.SoftDeleteAsset. The
-- restore gate reads it, so every soft-delete path has to write it.
UPDATE collections
SET deleted_at = NOW(), deleted_reason = $2, deleted_by_user_ref = $3, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetCollectionDeletedBy :one
-- Who soft-deleted this collection. pgx.ErrNoRows when the row is live
-- or absent — the two cases the restore path already conflates.
SELECT deleted_by_user_ref
  FROM collections
 WHERE id = $1 AND deleted_at IS NOT NULL;

-- name: ListCollectionsPage :many
-- NOT THE ENFORCEMENT PATH, and NOT dead either. It applies no
-- visibility predicate and no production code calls it — browse goes
-- through ListCollectionsPageGated (list_page.go), which splices
-- visibility.Predicate. It is retained as the PARITY ORACLE for
-- TestListCollectionsPage_FilterParity: every narg filter and the
-- (created_at DESC, id DESC) cursor are product behaviour that had to
-- survive the #449 hand-rewrite, and comparing the two implementations
-- over the same rows catches a filter bug that hand-written
-- expectations would encode twice. #661 proposed deleting it as dead;
-- it is not — deleting it deletes that test's oracle. Do not call it
-- from handler code.
--
-- Cursor pagination on (created_at DESC, id DESC). Filters are
-- nullable narg() so a single query covers every combo.
--
-- `q_name` is an optional case-insensitive substring match on the
-- collection's display name; the handler ILIKE-escapes the input.
--
-- `shared_with_user` powers the "Shared" hub tab: collections the
-- caller has an ACL grant on but doesn't own. The handler also passes
-- the caller's user_ref into `exclude_owner` to drop owned rows.
SELECT id, owner_user_ref, name, description, visibility, membership,
       expires_at, purpose, origin_server_id,
       created_at, updated_at, search_text, smart_query,
       deleted_at, deleted_reason, deleted_by_user_ref, cover_asset_id,
       featured_cover_asset_id, featured_cover_focal_x, featured_cover_focal_y,
       cover_focal_x, cover_focal_y
FROM collections c
WHERE (sqlc.narg('include_deleted')::BOOLEAN IS TRUE OR deleted_at IS NULL)
  AND (sqlc.narg('owner_user_ref')::BIGINT  IS NULL OR owner_user_ref = sqlc.narg('owner_user_ref')::BIGINT)
  AND (sqlc.narg('exclude_owner')::BIGINT   IS NULL OR owner_user_ref <> sqlc.narg('exclude_owner')::BIGINT)
  AND (sqlc.narg('visibility')::TEXT        IS NULL OR visibility     = sqlc.narg('visibility')::TEXT)
  AND (sqlc.narg('featured')::BOOLEAN       IS NULL OR sqlc.narg('featured')::BOOLEAN = EXISTS (
         SELECT 1 FROM featured_items fi
          WHERE fi.subject_kind = 'collection'
            AND fi.subject_id   = c.id
            -- The SIGNED-IN arm of featured.ScopeVisibleSQL (#1104).
            -- This is the parity oracle and sqlc queries are static
            -- strings, so it cannot splice the Go helper; the signed-in
            -- arm is the one the parity test exercises. Written
            -- byte-for-byte as the helper renders it, and
            -- TestScopeVisibleSQL_PinnedInStaticQueries fails the build
            -- if the two ever drift.
            AND fi.scope IN ('org', 'public')
       ))
  AND (sqlc.narg('q_name')::TEXT            IS NULL OR name ILIKE '%' || sqlc.narg('q_name')::TEXT || '%')
  AND (sqlc.narg('shared_with_user')::BIGINT IS NULL OR EXISTS (
         SELECT 1 FROM collection_acls a
          WHERE a.collection_id = c.id
            AND a.principal_type = 'user'
            AND a.principal_id   = sqlc.narg('shared_with_user')::BIGINT::TEXT
            AND (a.expires_at IS NULL OR a.expires_at > NOW())
       ))
  AND (sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL
       OR created_at < sqlc.narg('cursor_created_at')::TIMESTAMPTZ
       OR (created_at = sqlc.narg('cursor_created_at')::TIMESTAMPTZ
           AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: CountCollectionResources :one
SELECT COUNT(*)::BIGINT AS value
FROM collection_resources
WHERE collection_id = $1 AND pinned = TRUE
  AND (expires_at IS NULL OR expires_at > NOW());

-- ---------------------------------------------------------------------------
-- collection_resources (manual membership)
-- ---------------------------------------------------------------------------

-- name: AddCollectionResource :exec
-- Idempotent on the PK (collection_id, asset_id). A re-add updates
-- sort_order + expires_at + pinned but keeps added_at fixed.
INSERT INTO collection_resources (
    collection_id, asset_id, sort_order, pinned, expires_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (collection_id, asset_id) DO UPDATE SET
    sort_order = EXCLUDED.sort_order,
    pinned     = EXCLUDED.pinned,
    expires_at = EXCLUDED.expires_at;

-- name: RemoveCollectionResource :exec
DELETE FROM collection_resources WHERE collection_id = $1 AND asset_id = $2;

-- name: ListCollectionResourcesPage :many
-- NOT THE ENFORCEMENT PATH. Applies no visibility predicate; nothing in
-- production calls it. Collection contents go through
-- ListCollectionResourcesPageGated (resources_page.go), which splices
-- visibility.Predicate — sqlc's static SQL cannot take a runtime
-- fragment (#438). Retained for its generated row shape, which stays in
-- sync with the schema. Do not call it from handler code.
--
-- #661 proposed deleting it as dead. The QUERY is unreachable, but the
-- generated ListCollectionResourcesPageRow is load-bearing: the gated
-- row embeds it and resourceRowToAPI consumes it, so deleting the query
-- means hand-writing that struct and losing the schema sync this
-- comment relies on. Keeping it is the better trade.
-- Returns pinned members, sorted by sort_order then added_at. Excludes
-- expired-membership rows. Joined onto assets so the list can carry
-- the title/thumb/type the front-end needs without an N+1.
-- file_extension + thumbhash are part of that set (#595): a member tile
-- renders through the same CardThumb as browse, which derives the media
-- type (video / 3D badge + sprite-scrub hover) from the extension alone.
SELECT cr.collection_id, cr.asset_id, cr.sort_order, cr.pinned,
       cr.expires_at, cr.added_at,
       a.title, a.asset_type, a.status, a.file_hash,
       a.file_extension, a.thumbhash,
       a.created_at AS asset_created_at
FROM collection_resources cr
JOIN assets a ON a.id = cr.asset_id
WHERE cr.collection_id = $1
  AND cr.pinned = TRUE
  AND (cr.expires_at IS NULL OR cr.expires_at > NOW())
  AND a.deleted_at IS NULL
  AND (sqlc.narg('cursor_sort_order')::INTEGER IS NULL
       OR cr.sort_order > sqlc.narg('cursor_sort_order')::INTEGER
       OR (cr.sort_order = sqlc.narg('cursor_sort_order')::INTEGER
           AND cr.added_at > sqlc.narg('cursor_added_at')::TIMESTAMPTZ))
ORDER BY cr.sort_order ASC, cr.added_at ASC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- ---------------------------------------------------------------------------
-- ACLs (Phase 1.7.B-7c)
-- ---------------------------------------------------------------------------

-- name: ListCollectionAcls :many
SELECT collection_id, principal_type, principal_id, permission,
       granted_at, granted_by_user_ref, expires_at
FROM collection_acls
WHERE collection_id = $1
ORDER BY granted_at DESC, principal_type, principal_id, permission;

-- name: AddCollectionAcl :exec
INSERT INTO collection_acls (collection_id, principal_type, principal_id, permission,
                             granted_by_user_ref, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (collection_id, principal_type, principal_id, permission) DO UPDATE SET
    granted_at            = NOW(),
    granted_by_user_ref = EXCLUDED.granted_by_user_ref,
    expires_at            = EXCLUDED.expires_at;

-- name: RemoveCollectionAcl :execrows
DELETE FROM collection_acls
WHERE collection_id = $1
  AND principal_type = $2
  AND principal_id   = $3
  AND permission     = $4;
