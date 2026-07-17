-- Admin-only seed/loader queries — backfill timestamps + forge
-- comments. NOT for general operator use.

-- name: BackfillAssetTimestamps :execrows
-- Bulk UPDATE assets.created_at / updated_at from a JSONB
-- array of {id, created_at, updated_at} objects. JSONB +
-- jsonb_to_recordset keeps params simple (one arg) + lets
-- Postgres + sqlc infer types cleanly vs the multi-arg
-- unnest pattern.
UPDATE assets a
   SET created_at = u.created_at,
       updated_at = u.updated_at
  FROM jsonb_to_recordset(sqlc.arg('payload')::jsonb)
       AS u(id uuid,
            created_at timestamptz,
            updated_at timestamptz)
 WHERE a.id = u.id;

-- name: BackfillPostTimestamps :execrows
-- Same shape minus last_reviewed_at. Also touches posted_at so
-- the public feed ordering matches the seeded timeline (posts
-- order by posted_at, not created_at).
UPDATE posts p
   SET created_at = u.created_at,
       updated_at = u.updated_at,
       posted_at  = u.created_at
  FROM jsonb_to_recordset(sqlc.arg('payload')::jsonb)
       AS u(id uuid,
            created_at timestamptz,
            updated_at timestamptz)
 WHERE p.id = u.id;

-- name: BackfillCommentTimestamps :execrows
UPDATE comments c
   SET created_at = u.created_at,
       updated_at = u.updated_at
  FROM jsonb_to_recordset(sqlc.arg('payload')::jsonb)
       AS u(id uuid,
            created_at timestamptz,
            updated_at timestamptz)
 WHERE c.id = u.id;

-- name: SeedInsertUser :one
-- Forged username + optional password (NULL when omitted —
-- user can't log in but can be referenced as an actor on
-- posts/comments) + optional created_at. ON CONFLICT
-- (username) DO NOTHING + caller re-fetches via
-- SeedGetUserByUsername when 0 rows returned — that's how
-- idempotent re-runs surface the existing row to the client.
INSERT INTO "user" (
    username, password, fullname, email, usergroup, approved, created
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (username) DO NOTHING
RETURNING ref, username, fullname, email, usergroup, approved, created;

-- name: SeedGetUserByUsername :one
-- Re-fetch path for idempotent SeedInsertUser conflicts.
SELECT ref, username, fullname, email, usergroup, approved, created
FROM "user"
WHERE username = $1;

-- name: SeedAuthorExists :one
SELECT 1 AS ok FROM "user" WHERE ref = $1;

-- name: SeedPostExists :one
SELECT 1 AS ok FROM posts WHERE id = $1 AND deleted_at IS NULL;

-- name: SeedAssetExists :one
SELECT 1 AS ok FROM assets WHERE id = $1;

-- name: SeedCollectionExists :one
SELECT 1 AS ok FROM collections WHERE id = $1;

-- name: SeedGetCommentByID :one
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at,
       peer_id, actor_uri, activity_uri
FROM comments
WHERE id = $1;

-- name: SeedGetCommentParentInfo :one
-- Fetches root_id + depth from a parent comment so the new row's
-- thread placement matches the public CreateComment path's
-- semantics.
SELECT root_id, depth
FROM comments
WHERE id = $1 AND deleted_at IS NULL;

-- name: SeedInsertComment :one
-- Forged-author + optional created_at + optional stable id.
-- ON CONFLICT (id) DO NOTHING + the caller re-fetches via
-- SeedGetCommentByID when 0 rows returned — that's how
-- idempotent re-runs surface the existing row to the client.
INSERT INTO comments (
    id, target_kind, target_id, parent_id, root_id, depth,
    author_user_ref, body, body_html,
    annotation_type, annotation_data,
    created_at, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
ON CONFLICT (id) DO NOTHING
RETURNING id, target_kind, target_id, parent_id, root_id, depth,
          author_user_ref, body, body_html,
          annotation_type, annotation_data,
          like_count, edited_at, deleted_at,
          origin_server_id, created_at, updated_at,
          peer_id, actor_uri, activity_uri;

-- =========================================================================
-- `aa seed` DB-direct loader queries (issue #321). These replace the
-- HTTP round-trips apply.py used to make; the seeder walks the same
-- dependency-ordered phases but writes straight to the tables. All
-- inserts are idempotent (ON CONFLICT DO NOTHING) so a re-run against
-- a partially-seeded DB is a no-op rather than a hard error.
-- =========================================================================

-- name: SeedGetUserRefByUsername :one
-- Lean username -> ref lookup used to resolve the bootstrap admin
-- (collection owner) + asset/post authors that were created in an
-- earlier run (so a resumed seed re-hydrates its ID map).
SELECT ref FROM "user" WHERE username = $1;

-- name: SeedListWorkflowStates :many
-- Every workflow state, keyed by (domain, code) in the seeder. The
-- baseline migration seeds domains 'asset:1' + 'post'; the seeder
-- collapses the richer dataset state names onto these.
SELECT id, domain, code FROM workflow_states;

-- name: SeedListAssetTypes :many
-- asset_type registry (Image=1, Document=2, ... seeded by baseline).
-- The seeder maps the dataset's typed-folder labels onto these refs.
SELECT ref, name FROM asset_types WHERE name IS NOT NULL;

-- name: SeedInsertTeam :one
-- Stable team id comes from dataset.teams.json. ON CONFLICT on the
-- (origin_server_id, slug) unique key AND the id pkey are both
-- possible on a resumed run; a bare DO NOTHING catches either.
INSERT INTO teams (id, slug, name, description)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING
RETURNING id;

-- name: SeedGetTeamBySlug :one
SELECT id FROM teams WHERE slug = $1 AND deleted_at IS NULL;

-- name: SeedInsertTeamClosureSelf :exec
-- Every team is its own depth-0 ancestor. The team hierarchy
-- queries assume this self-row exists (matches the POST /teams path).
INSERT INTO team_closure (ancestor_id, descendant_id, depth)
VALUES ($1, $1, 0)
ON CONFLICT DO NOTHING;

-- name: SeedInsertTeamMembership :exec
INSERT INTO team_memberships (team_id, user_ref)
VALUES ($1, $2)
ON CONFLICT (team_id, user_ref) DO NOTHING;

-- name: SeedInsertField :one
-- Field definition. `code` is the federation-stable natural key.
INSERT INTO field_definition (
    code, label, type, options, required, searchable, applies_to, subject_kind
)
VALUES ($1, $2, $3, $4, false, true, '{}'::bigint[], 'asset')
ON CONFLICT (code) DO NOTHING
RETURNING id;

-- name: SeedGetFieldByCode :one
SELECT id FROM field_definition WHERE code = $1;

-- name: SeedInsertCollection :one
-- Stable id from dataset.collections.json; owner is the bootstrap
-- admin (the dataset carries no per-collection owner).
INSERT INTO collections (id, owner_user_ref, name, description, visibility)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING
RETURNING id;

-- name: SeedSetCollectionFeatured :exec
-- Sets the per-collection browse-filter flag (collections.featured),
-- which is what the PUBLIC /collections "featured" tab filters on —
-- distinct from the featured_items curation rail the admin page reads
-- (see 00002_featured_items.sql). Idempotent by nature (UPDATE), so a
-- re-seed is a no-op. Only ever sets TRUE; unflagging a collection in
-- the catalogue takes effect on the next full reset, which is how the
-- demo re-seeds.
UPDATE collections SET featured = TRUE WHERE id = $1;

-- name: SeedInsertFeatured :exec
-- Feature one collection/asset on the homepage + /admin/featured (#380).
-- Position appends after any existing rows (max+1) — the SAME rule as
-- the admin InsertFeaturedItem (internal/featured/queries.sql), so
-- seeded rows and later admin curation interleave in one order. The
-- (subject_kind, subject_id) unique constraint + ON CONFLICT DO NOTHING
-- make a re-seed a no-op: no duplicates, and no position drift because
-- the conflicting row is already counted in MAX(position).
INSERT INTO featured_items (subject_kind, subject_id, position, created_by_user_ref)
VALUES (
    $1,
    $2,
    (SELECT COALESCE(MAX(position), -1) + 1 FROM featured_items),
    $3
)
ON CONFLICT (subject_kind, subject_id) DO NOTHING;

-- name: SeedInsertAsset :one
-- Stable id from the MANIFEST. A bare ON CONFLICT DO NOTHING catches
-- both the id pkey (resumed run) AND the (owner_user_ref, file_hash)
-- partial unique index (byte-identical duplicate owned by the same
-- user) — the latter legitimately collapses one asset, matching the
-- product's content-address invariant.
INSERT INTO assets (
    id, title, description, asset_type, owner_user_ref, status,
    file_hash, file_extension, file_size_bytes, metadata,
    state_id, team_id, sensitivity, created_at, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT DO NOTHING
RETURNING id;

-- name: SeedInsertAssetTag :exec
INSERT INTO asset_tag (asset_id, tag, source)
VALUES ($1, $2, 'import')
ON CONFLICT (asset_id, tag) DO NOTHING;

-- name: SeedInsertCollectionResource :exec
INSERT INTO collection_resources (collection_id, asset_id)
VALUES ($1, $2)
ON CONFLICT (collection_id, asset_id) DO NOTHING;

-- name: SeedInsertAssetFieldValue :exec
-- Multi-type value column carried by the caller per the field's
-- declared type (mirrors metadata handler.go's typeFromFieldDef).
INSERT INTO asset_field_value (
    asset_id, field_id, value_text, value_num, value_date, value_options, value_ref, set_by
)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'import')
ON CONFLICT (asset_id, field_id) DO NOTHING;

-- name: SeedInsertPost :one
-- Stable id from posts.json. author_user_ref resolved from
-- author_username. cover set to the first resolved member asset.
--
-- cover_thumbnail_asset_id is left NULL on purpose (#355). It means
-- "optional STANDALONE thumbnail asset, NOT a member of the post" — an
-- override for when an uploader supplies a separate cover image. The
-- seed dataset has no such standalone covers, so the honest value is
-- NULL; pointing it at the cover (a member) both contradicted the
-- field's contract and made every seeded post look like it carried a
-- custom thumbnail. Cards read cover_asset_id and render its `col`
-- variant, which the preview dispatch now produces.
INSERT INTO posts (
    id, author_user_ref, title, description, visibility,
    cover_asset_id, cover_thumbnail_asset_id, state_id, team_id,
    created_at, updated_at, posted_at
)
VALUES ($1, $2, $3, $4, $5, $6, NULL, $7, $8, $9, $10, $9)
ON CONFLICT (id) DO NOTHING
RETURNING id;

-- name: SeedInsertPostAsset :exec
INSERT INTO post_assets (post_id, asset_id, sort_order)
VALUES ($1, $2, $3)
ON CONFLICT (post_id, asset_id) DO NOTHING;

-- name: SeedInsertPostTag :exec
INSERT INTO post_tags (post_id, tag)
VALUES ($1, $2)
ON CONFLICT (post_id, tag) DO NOTHING;

-- name: SeedInsertCollectionPost :exec
INSERT INTO collection_posts (collection_id, post_id)
VALUES ($1, $2)
ON CONFLICT (collection_id, post_id) DO NOTHING;
