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
-- extraction_source/mode carry the #618 wiring; NULLIF/COALESCE keeps
-- the column defaults for the operator-managed fields that pass ''.
INSERT INTO field_definition (
    code, label, type, options, required, searchable, applies_to, subject_kind,
    extraction_source, extraction_mode
)
VALUES ($1, $2, $3, $4, false, true, '{}'::bigint[], 'asset',
        $5, COALESCE(NULLIF(sqlc.arg(extraction_mode)::text, ''), 'skip_if_set'))
ON CONFLICT (code) DO NOTHING
RETURNING id;

-- name: SeedGetFieldByCode :one
-- Recovery path for SeedInsertField's ON CONFLICT DO NOTHING. `type`
-- is selected alongside the id because the row the catalogue binds to
-- may be typed differently from the catalogue entry that bound to it
-- (#812): the seed must write values against the type the COLUMN
-- actually has, not the one the JSON claims.
SELECT id, type FROM field_definition WHERE code = $1;

-- name: SeedListFields :many
-- Every field definition that already exists, so a manifest can write a
-- value for one the seed CATALOGUE does not mention (#820).
--
-- applyFields used to build its code→id map from dataset.field_definitions.json
-- alone, so `r.fields` held exactly the 20 studio codes. The nine codes
-- the MIGRATIONS ship — title, description, credit, copyright,
-- capture_date, keywords, country, pixel_width, pixel_height — were
-- absent from that map even though the rows were sitting in the table,
-- and every manifest value naming one was thrown away as
-- `seed.field.unknown_code`. Before #812 that was invisible, because
-- `aa seed --reset` TRUNCATEd the shipped rows anyway; since #812 they
-- survive the reset and the map is simply missing them.
--
-- Ordered by code so the log line and any drop tally are reproducible.
SELECT id, code, type FROM field_definition ORDER BY code;

-- name: SeedInsertCollection :one
-- Stable id from dataset.collections.json; owner is the bootstrap
-- admin (the dataset carries no per-collection owner).
INSERT INTO collections (id, owner_user_ref, name, description, visibility)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING
RETURNING id;

-- name: SeedInsertFeatured :exec
-- Feature one collection/asset on the public rail + /admin/featured
-- (#380, ADR 0065).
--
-- scope='public' because the demo's flagged collections are exactly
-- what a public rail should show — a freshly-reset demo whose landing
-- page is empty is the thing this phase exists to prevent, and since
-- #416 the rail IS the landing page for a logged-out visitor.
--
-- Position appends after any existing rows on THIS SURFACE (max+1) —
-- the SAME rule as the admin InsertFeaturedItem
-- (internal/featured/queries.sql), so seeded rows and later admin
-- curation interleave in one order. `band_id IS NULL` scopes it to the
-- rail (#1118): a global MAX would hand a seeded rail row a position
-- derived from a promo band's ordering.
--
-- ⛔ THE ON CONFLICT TARGET MUST NAME EVERY COLUMN OF
-- featured_items_placement_unique, AND THIS HAS NOW BROKEN TWICE.
--
-- An ON CONFLICT target is matched against a real constraint by its
-- exact column list; a list that matches nothing is a RUNTIME error,
-- 42P10 "there is no unique or exclusion constraint matching the ON
-- CONFLICT specification". It is not a compile error, sqlc does not
-- check it, and no Go test reaches this statement — the only thing that
-- executes it is `aa seed` against a real dataset, which is why both
-- failures surfaced in the Playwright job's seed step rather than in
-- the suite.
--
--   00010 widened (subject_kind, subject_id)
--              → (subject_kind, subject_id, scope, team_id)
--   00053 widened it again, adding band_id (#1118)
--
-- The 00010 note said "that rename is not cosmetic" and #1118 missed
-- this file anyway, so the note is now an instruction instead of an
-- observation: WHEN YOU CHANGE featured_items_placement_unique, GREP
-- FOR `ON CONFLICT` UNDER app/ AND FIX EVERY TARGET NAMING THIS TABLE.
-- Today that is exactly this statement — migration 00010's own insert
-- uses a bare `ON CONFLICT DO NOTHING`, which infers no constraint and
-- therefore survives any widening, and internal/featured's insert has
-- no target at all (its 23505 is caught in Go and mapped to 409).
INSERT INTO featured_items (subject_kind, subject_id, position, created_by_user_ref, scope)
VALUES (
    $1,
    $2,
    (SELECT COALESCE(MAX(position), -1) + 1 FROM featured_items f2
      WHERE f2.band_id IS NULL),
    $3,
    'public'
)
ON CONFLICT (subject_kind, subject_id, scope, team_id, band_id) DO NOTHING;

-- name: SeedInsertAsset :one
-- Stable id from the MANIFEST. `mature` is carried from the manifest
-- entry (#1217): it is a LABEL the catalogue authors, orthogonal to
-- `sensitivity` (ADR 0090), so the seeder writes it here rather than
-- deriving it from the tier. Posts get theirs from the 00052/00054
-- trigger when their membership lands — never written directly.
--
-- `ai_provenance` is carried the same way and for the same reason
-- (#1251 slice 3, ADR 0094): it is the MAKER'S DECLARATION, a fact the
-- catalogue authors, and NULL — the overwhelming majority — means
-- UNDECLARED rather than `none`. Posts get their two derived AI facts
-- (`ai_provenance`, `ai_pure`) from the 00060/00061 triggers when their
-- membership and covers land, never written directly, exactly as
-- `mature` works one column over.
--
-- ⚠️ THE ORDER OF THE PHASES IS WHAT MAKES THAT WORK. applyAssets runs
-- before applyPosts, so every declaration is already on the asset row
-- when `post_assets` is written and the recompute fires with the real
-- population. A declaration written AFTER a post exists would still be
-- correct — the trigger on `assets` covers that path — but the seed
-- never takes it.
--
-- A bare ON CONFLICT DO NOTHING catches
-- both the id pkey (resumed run) AND the (owner_user_ref, file_hash)
-- partial unique index (byte-identical duplicate owned by the same
-- user) — the latter legitimately collapses one asset, matching the
-- product's content-address invariant.
--
-- ⛔ THAT COLLAPSE IS WHY A DECLARATION IS NOT A PER-POST KNOB. One
-- asset row can be a member of MANY posts, so declaring a shared asset
-- `generated` moves every post containing it. A catalogue entry that
-- wants to move exactly one post has to name an asset unique to it.
INSERT INTO assets (
    id, title, description, asset_type, owner_user_ref, status,
    file_hash, file_extension, file_size_bytes, metadata,
    state_id, team_id, sensitivity, mature, ai_provenance,
    created_at, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
ON CONFLICT DO NOTHING
RETURNING id;

-- name: SeedInsertAssetCompanion :exec
-- Attach a companion blob (bin buffer / texture / .mtl) to a seeded
-- asset under its declared relative path so multi-file glTF/OBJ models
-- resolve their siblings at render + view time (#486). Companion bytes
-- live in storage_objects (deduped by hash); this row is the
-- asset+path→blob mapping. Idempotent for resumed seeds.
INSERT INTO asset_companions (
    asset_id, companion_path, object_hash, content_type, size_bytes
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (asset_id, companion_path) DO NOTHING;

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

-- name: SeedInsertFollow :exec
-- Local-origin follow edge for the seeded social graph (#563).
-- origin_server_id is NULL — the seed graph is local, never federated
-- (ADR 0007). Idempotent on the (follower, followee) primary key so
-- re-seeds are byte-stable.
INSERT INTO user_follows (follower_user_ref, followee_user_ref, created_at, origin_server_id)
VALUES ($1, $2, $3, NULL)
ON CONFLICT DO NOTHING;

-- name: SeedInsertLike :exec
-- Local-origin like for the seeded like history (#563). user_ref is set;
-- peer_id/actor_uri stay NULL (the likes_origin_check enforces exactly
-- one origin). target_kind is 'post' here. The likes_after_insert
-- trigger maintains posts.like_count, so the count always equals the
-- row set — no separate like_count write. Idempotent (the local unique
-- index (target_kind, target_id, user_ref)) so re-seeds are stable.
INSERT INTO likes (target_kind, target_id, user_ref, liked_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: SeedFindRoleByName :one
-- The shipped role a seeded test-fixture principal is given (#1270).
--
-- The registration endpoint assigns the configured default role — "Base"
-- unless an operator changed it — and the four accounts this replaces
-- were REGISTERED, so they had one. `AdminHandler.CreateUser` assigns
-- none: the 31 fictional artists cannot log in and never needed caps.
-- A principal seeded with no role would sign in and then be refused
-- every write the spec drives, which reads as a permission regression
-- and is a missing fixture.
SELECT id FROM roles WHERE name = $1 LIMIT 1;

-- name: SeedSetUserGlobalRole :exec
-- Same statement auth.SetUserGlobalRole runs, so a seeded principal and
-- a registered one end up with identical role state. Global only
-- (team_id IS NULL); team-scoped assignments are untouched. Atomic at
-- statement level, so there is no window where the user has zero roles.
WITH _del AS (
    DELETE FROM user_roles
     WHERE user_ref = $1 AND team_id IS NULL
)
INSERT INTO user_roles (user_ref, role_id, assigned_by_user_ref)
VALUES ($1, $2, $3)
ON CONFLICT ON CONSTRAINT user_roles_unique DO UPDATE SET
    assigned_at          = NOW(),
    assigned_by_user_ref = EXCLUDED.assigned_by_user_ref;
