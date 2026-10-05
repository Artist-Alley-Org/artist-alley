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
          cover_focal_x, cover_focal_y,
          featured_cover_zoom, cover_zoom;

-- name: GetCollection :one
-- Filters soft-deleted rows by default. Admin surfaces reading
-- deleted rows use GetCollectionIncludingDeleted below.
SELECT id, owner_user_ref, name, description, visibility, membership,
       expires_at, purpose, origin_server_id,
       created_at, updated_at, search_text, smart_query,
       deleted_at, deleted_reason, deleted_by_user_ref, cover_asset_id,
       featured_cover_asset_id, featured_cover_focal_x, featured_cover_focal_y,
       cover_focal_x, cover_focal_y,
       featured_cover_zoom, cover_zoom
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
       cover_focal_x, cover_focal_y,
       featured_cover_zoom, cover_zoom
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
--
-- #1333 adds a FOURTH state to every crop column: "the picture you were
-- describing is gone". A focal fraction and a zoom multiplier are both
-- chosen against one specific photograph, so they mean nothing on the
-- next one; left alone across a cover swap they framed the new picture
-- on a point nobody picked, and did it silently. Each crop column
-- therefore reads its own cover column, and the arms are ordered:
--   1. the column's explicit clear flag wins outright;
--   2. a SUPPLIED value wins over the swap rule. This is the case both
--      cover editors actually take, because they save the new picture
--      and its new framing in ONE PATCH, so a rule that cleared on any
--      change would discard the value the curator just chose while
--      still passing a single-field test;
--   3. the cover being removed, or genuinely CHANGED, clears the crop.
--      `IS DISTINCT FROM` rather than `IS NOT NULL`, so a client that
--      round-trips the whole object and re-sends the SAME cover id is
--      not a swap and keeps its framing;
--   4. otherwise the stored value stands, exactly as before.
-- Both axes of a focal pair read identical arms, so the pair can never
-- half-clear into a collections_cover_focal_check violation. The two
-- SLOTS stay independent: swapping the featured picture must not
-- disturb how the collection card is framed, and vice versa.
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
    -- #1333: the swap arm. See the note above UpdateCollection for why
    -- the arms are in this order; the short version is that arm 2 is
    -- what keeps the editor's "new picture AND its new framing in one
    -- PATCH" working, and arm 3 is what stops a framing outliving the
    -- picture it was chosen against.
    featured_cover_focal_x = CASE
        WHEN sqlc.arg('clear_featured_cover_focal')::BOOLEAN THEN NULL
        WHEN sqlc.narg('featured_cover_focal_x')::DOUBLE PRECISION IS NOT NULL
             THEN sqlc.narg('featured_cover_focal_x')::DOUBLE PRECISION
        WHEN sqlc.arg('clear_featured_cover')::BOOLEAN THEN NULL
        WHEN sqlc.narg('featured_cover_asset_id')::UUID IS NOT NULL
             AND sqlc.narg('featured_cover_asset_id')::UUID IS DISTINCT FROM featured_cover_asset_id
             THEN NULL
        ELSE featured_cover_focal_x END,
    featured_cover_focal_y = CASE
        WHEN sqlc.arg('clear_featured_cover_focal')::BOOLEAN THEN NULL
        WHEN sqlc.narg('featured_cover_focal_y')::DOUBLE PRECISION IS NOT NULL
             THEN sqlc.narg('featured_cover_focal_y')::DOUBLE PRECISION
        WHEN sqlc.arg('clear_featured_cover')::BOOLEAN THEN NULL
        WHEN sqlc.narg('featured_cover_asset_id')::UUID IS NOT NULL
             AND sqlc.narg('featured_cover_asset_id')::UUID IS DISTINCT FROM featured_cover_asset_id
             THEN NULL
        ELSE featured_cover_focal_y END,
    -- The regular cover's own focal pair, on the 4:3 collection-card
    -- destination (#1334: NOT a square; `col` is a square SOURCE and
    -- CollectionCard's tile is what actually crops). Its own clear flag,
    -- for the reason the featured pair has one: two columns, one
    -- intention. And its own #1333 swap arms, keyed on ITS cover column.
    cover_focal_x = CASE
        WHEN sqlc.arg('clear_cover_focal')::BOOLEAN THEN NULL
        WHEN sqlc.narg('cover_focal_x')::DOUBLE PRECISION IS NOT NULL
             THEN sqlc.narg('cover_focal_x')::DOUBLE PRECISION
        WHEN sqlc.arg('clear_cover')::BOOLEAN THEN NULL
        WHEN sqlc.narg('cover_asset_id')::UUID IS NOT NULL
             AND sqlc.narg('cover_asset_id')::UUID IS DISTINCT FROM cover_asset_id
             THEN NULL
        ELSE cover_focal_x END,
    cover_focal_y = CASE
        WHEN sqlc.arg('clear_cover_focal')::BOOLEAN THEN NULL
        WHEN sqlc.narg('cover_focal_y')::DOUBLE PRECISION IS NOT NULL
             THEN sqlc.narg('cover_focal_y')::DOUBLE PRECISION
        WHEN sqlc.arg('clear_cover')::BOOLEAN THEN NULL
        WHEN sqlc.narg('cover_asset_id')::UUID IS NOT NULL
             AND sqlc.narg('cover_asset_id')::UUID IS DISTINCT FROM cover_asset_id
             THEN NULL
        ELSE cover_focal_y END,
    -- #1212 — how far each crop is tightened. One column per slot and
    -- one clear flag per column, and the flag is NOT optional dressing
    -- on a numeric field: NULL means "leave alone" here exactly as it
    -- does above, so without the CASE a curator who zoomed and then
    -- reset would get a 200 and an unchanged column — #1073's silent
    -- non-clear, on a new pair of columns. It is a SEPARATE flag from
    -- the focal pair's because zoom and position are independent
    -- settings: "back to fit, still positioned left" is an ordinary
    -- thing to want, and one shared flag could not say it.
    --
    -- The swap arms are here too (#1333), and they are not optional
    -- tidiness: zoom and focal together ARE the crop. Clearing only the
    -- focal on a cover swap would leave the new picture centred but
    -- still tightened to 3x by a decision taken about a different
    -- photograph, which is the same silent wrongness one column over.
    featured_cover_zoom = CASE
        WHEN sqlc.arg('clear_featured_cover_zoom')::BOOLEAN THEN NULL
        WHEN sqlc.narg('featured_cover_zoom')::DOUBLE PRECISION IS NOT NULL
             THEN sqlc.narg('featured_cover_zoom')::DOUBLE PRECISION
        WHEN sqlc.arg('clear_featured_cover')::BOOLEAN THEN NULL
        WHEN sqlc.narg('featured_cover_asset_id')::UUID IS NOT NULL
             AND sqlc.narg('featured_cover_asset_id')::UUID IS DISTINCT FROM featured_cover_asset_id
             THEN NULL
        ELSE featured_cover_zoom END,
    cover_zoom = CASE
        WHEN sqlc.arg('clear_cover_zoom')::BOOLEAN THEN NULL
        WHEN sqlc.narg('cover_zoom')::DOUBLE PRECISION IS NOT NULL
             THEN sqlc.narg('cover_zoom')::DOUBLE PRECISION
        WHEN sqlc.arg('clear_cover')::BOOLEAN THEN NULL
        WHEN sqlc.narg('cover_asset_id')::UUID IS NOT NULL
             AND sqlc.narg('cover_asset_id')::UUID IS DISTINCT FROM cover_asset_id
             THEN NULL
        ELSE cover_zoom END,
    updated_at  = NOW()
WHERE id = sqlc.arg('id')
RETURNING id, owner_user_ref, name, description, visibility, membership,
          expires_at, purpose, origin_server_id,
          created_at, updated_at, search_text, smart_query,
          deleted_at, deleted_reason, deleted_by_user_ref, cover_asset_id,
          featured_cover_asset_id, featured_cover_focal_x, featured_cover_focal_y,
          cover_focal_x, cover_focal_y,
          featured_cover_zoom, cover_zoom;

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
       cover_focal_x, cover_focal_y,
       featured_cover_zoom, cover_zoom
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
